local config = require("bingo.config")
local health = require("bingo.health")

local M = {}
local Manager = {}
Manager.__index = Manager

local probe_timeout_ms = 1000
local poll_interval_ms = 100

local function default_dependencies()
  local uv = vim.uv or vim.loop
  return {
    uv = uv,
    schedule = vim.schedule,
    executable = vim.fn.executable,
    exepath = vim.fn.exepath,
    mkdir = function(path)
      vim.fn.mkdir(path, "p")
    end,
    notify = function(message)
      vim.notify(message, vim.log.levels.ERROR, { title = "bingo" })
    end,
    stdpath = vim.fn.stdpath,
    probe = health.probe,
  }
end

local function source_root()
  local source = debug.getinfo(1, "S").source
  if source:sub(1, 1) == "@" then
    source = source:sub(2)
  end
  local dirname = vim.fs.dirname
  return dirname(dirname(dirname(vim.fn.fnamemodify(source, ":p"))))
end

local plugin_root = source_root()

local function supported_platform(uv)
  local uname = uv.os_uname()
  local system = uname.sysname
  local machine = uname.machine
  if system == "Darwin" and (machine == "arm64" or machine == "aarch64") then
    return true
  end
  return system == "Linux" and (machine == "x86_64" or machine == "amd64")
end

local function describe_probe(result)
  if result == nil then
    return "no health response"
  end
  if result.kind == "transportError" then
    return "last health error: " .. tostring(result.error)
  end
  if result.kind == "incompatible" then
    return "last health response: " .. result.reason
  end
  return "last health result: " .. result.kind
end

function M.new(options, dependencies)
  return setmetatable({
    options = options,
    deps = dependencies or default_dependencies(),
    in_flight = {},
    timers = {},
    processes = {},
    disposed = false,
  }, Manager)
end

function Manager:_log(message)
  if self.options.log ~= nil then
    self.options.log(message)
  end
end

function Manager:_complete(key, error_message, endpoint, attempt)
  local waiters = self.in_flight[key]
  if waiters == nil or (attempt ~= nil and waiters ~= attempt) then
    return
  end
  self.in_flight[key] = nil
  if waiters.probe and waiters.probe.cancel then
    waiters.probe.cancel()
  end
  waiters.probe = nil
  if waiters.timer then
    local timer = waiters.timer
    self.timers[timer] = nil
    if not timer:is_closing() then
      timer:stop()
      timer:close()
    end
    waiters.timer = nil
  end
  for _, callback in ipairs(waiters) do
    local ok, callback_error = pcall(callback, error_message, endpoint)
    if not ok then
      pcall(
        self._log,
        self,
        "bingo server completion callback failed: " .. tostring(callback_error)
      )
      if self.deps.notify ~= nil then
        pcall(
          self.deps.notify,
          "bingo server completion callback failed: " .. tostring(callback_error)
        )
      end
    end
  end
end

function Manager:_guard(key, attempt, callback)
  if self.in_flight[key] ~= attempt then
    return
  end
  local ok, startup_error = pcall(callback)
  if not ok then
    self:_complete(
      key,
      "bingo server startup failed: " .. tostring(startup_error),
      nil,
      attempt
    )
  end
end

function Manager:_probe(key, attempt, resolved, timeout, callback)
  local probe = {}
  attempt.probe = probe
  local cancel = self.deps.probe(resolved.management, resolved.dap, timeout, function(result)
    if self.in_flight[key] ~= attempt or attempt.probe ~= probe then
      return
    end
    attempt.probe = nil
    callback(result)
  end)
  if attempt.probe == probe then
    probe.cancel = cancel
  elseif cancel then
    cancel()
  end
end

function Manager:_later(delay_ms, callback, attempt)
  local timer, timer_error = self.deps.uv.new_timer()
  if timer == nil then
    return "cannot allocate readiness timer: " .. tostring(timer_error)
  end
  self.timers[timer] = true
  attempt.timer = timer
  local started, start_error = timer:start(delay_ms, 0, function()
    if self.timers[timer] == nil then
      return
    end
    timer:stop()
    timer:close()
    self.timers[timer] = nil
    attempt.timer = nil
    self.deps.schedule(callback)
  end)
  if started == nil and start_error ~= nil then
    return "cannot start readiness timer: " .. tostring(start_error)
  end
end

function Manager:_resolve_binary(resolved)
  local explicit = resolved.binary
  if explicit ~= nil then
    local found = self.deps.exepath(explicit)
    if found ~= "" then
      return found
    end
    if self.deps.executable(explicit) == 1 then
      return explicit
    end
    return nil, "configured bingo binary is not executable: " .. explicit
  end

  local bundled = vim.fs.joinpath(plugin_root, "bin", "bingo")
  if self.deps.executable(bundled) == 1 then
    return bundled
  end
  local path_binary = self.deps.exepath("bingo")
  if path_binary ~= "" then
    return path_binary
  end
  return nil,
    "no bingo server binary found; run the plugin manager build hook or `bash editors/neovim/scripts/prepare.sh` from a checkout, put bingo on PATH, or set server.binary"
end

function M.inspect(options, dependencies)
  local resolved = config.resolve({}, options)
  local manager = M.new(options, dependencies)
  local info = {
    mode = resolved.mode,
    management = config.endpoint(resolved.management),
    dap = config.endpoint(resolved.dap),
  }
  if resolved.mode == "auto" then
    info.platform_supported = supported_platform(manager.deps.uv)
    info.platform = manager.deps.uv.os_uname().sysname
    info.binary, info.binary_error = manager:_resolve_binary(resolved)
    info.prepare_script = vim.fs.joinpath(plugin_root, "scripts", "prepare.sh")
  end
  return info
end

function Manager:_log_path(resolved)
  if resolved.log_path ~= nil then
    return resolved.log_path
  end
  return vim.fs.joinpath(self.deps.stdpath("state"), "bingo", "server.log")
end

function Manager:_spawn(resolved)
  local binary, binary_error = self:_resolve_binary(resolved)
  if self.disposed then
    return nil, "bingo server startup was cancelled"
  end
  if binary == nil then
    return nil, binary_error
  end

  local log_path = self:_log_path(resolved)
  local mkdir_ok, mkdir_error = pcall(self.deps.mkdir, vim.fs.dirname(log_path))
  if not mkdir_ok then
    return nil,
      "cannot create bingo server log directory for "
        .. log_path
        .. ": "
        .. tostring(mkdir_error)
  end
  if self.disposed then
    return nil, "bingo server startup was cancelled"
  end
  local log_fd, open_error = self.deps.uv.fs_open(log_path, "a", 420)
  if log_fd == nil then
    return nil, "cannot open bingo server log " .. log_path .. ": " .. tostring(open_error)
  end
  if self.disposed then
    local closed, close_error = self.deps.uv.fs_close(log_fd)
    if closed == nil and close_error then
      return nil, "cannot close bingo server log: " .. tostring(close_error)
    end
    return nil, "bingo server startup was cancelled"
  end

  local args = {
    "-addr",
    config.endpoint(resolved.management),
    "-dap-addr",
    config.endpoint(resolved.dap),
    "-idle-timeout",
    tostring(resolved.idle_timeout_ms) .. "ms",
  }
  local handle
  local pid
  local spawn_ok, spawn_error
  spawn_ok, handle, pid = pcall(
    self.deps.uv.spawn,
    binary,
    {
      args = args,
      detached = true,
      stdio = { nil, log_fd, log_fd },
    },
    function(code, signal)
      if handle ~= nil and not handle:is_closing() then
        handle:close()
      end
      self.deps.schedule(function()
        self.processes[pid] = nil
        if not self.disposed then
          self:_log(
            string.format(
              "managed bingo server %s exited with code %d signal %d",
              tostring(pid),
              code,
              signal
            )
          )
        end
      end)
    end
  )
  if not spawn_ok then
    spawn_error = handle
    handle = nil
  end
  local closed, close_error = self.deps.uv.fs_close(log_fd)
  if closed == nil and close_error then
    if handle and not handle:is_closing() then
      handle:close()
    end
    return nil, "cannot close bingo server log: " .. tostring(close_error)
  end
  if handle == nil then
    return nil,
      "cannot start bingo server: "
        .. tostring(spawn_error or pid)
        .. "; logs: "
        .. log_path
  end

  handle:unref()
  if self.disposed then
    if not handle:is_closing() then
      handle:close()
    end
    return nil, "bingo server startup was cancelled"
  end
  self.processes[pid] = handle
  self:_log("starting managed bingo server; logs: " .. log_path)
  return { pid = pid, log_path = log_path }
end

function Manager:_poll_ready(key, resolved, child, deadline_ms, last_result, attempt)
  if self.disposed or self.in_flight[key] ~= attempt then
    return
  end
  local now_ms = self.deps.uv.hrtime() / 1000000
  local remaining = deadline_ms - now_ms
  if remaining <= 0 then
    self:_complete(
      key,
      string.format(
        "managed bingo server did not become ready at %s within %dms (%s); DAP %s; logs: %s",
        config.endpoint(resolved.management),
        resolved.ready_timeout_ms,
        describe_probe(last_result),
        config.endpoint(resolved.dap),
        child.log_path
      ),
      nil,
      attempt
    )
    return
  end

  self:_probe(
    key,
    attempt,
    resolved,
    math.min(probe_timeout_ms, math.max(1, math.floor(remaining))),
    function(result)
      self:_guard(key, attempt, function()
        if self.disposed or self.in_flight[key] ~= attempt then
          return
        end
        if self.deps.uv.hrtime() / 1000000 >= deadline_ms then
          self:_poll_ready(key, resolved, child, deadline_ms, result, attempt)
          return
        end
        if result.kind == "compatible" then
          self:_log("reusing compatible bingo instance " .. result.health.instance_id)
          self:_complete(key, nil, resolved.dap, attempt)
          return
        end
        if result.kind == "incompatible" then
          self:_complete(
            key,
            "cannot use bingo management endpoint "
              .. config.endpoint(resolved.management)
              .. ": "
              .. result.reason,
            nil,
            attempt
          )
          return
        end
        local poll_remaining = deadline_ms - self.deps.uv.hrtime() / 1000000
        local timer_error = self:_later(math.max(1, math.min(poll_interval_ms, poll_remaining)), function()
          self:_guard(key, attempt, function()
            self:_poll_ready(key, resolved, child, deadline_ms, result, attempt)
          end)
        end, attempt)
        if timer_error then
          self:_complete(key, timer_error, nil, attempt)
        end
      end)
    end
  )
end

function Manager:_ensure_auto(key, resolved, attempt)
  if resolved.management.host ~= "127.0.0.1" or resolved.dap.host ~= "127.0.0.1" then
    self:_complete(
      key,
      'serverMode "auto" requires managementHost and dapHost to be 127.0.0.1; use "connectOnly" for remote or custom endpoints',
      nil,
      attempt
    )
    return
  end
  if not supported_platform(self.deps.uv) then
    self:_complete(
      key,
      'bingo server autostart supports only linux/amd64 and darwin/arm64; use serverMode "connectOnly" with an existing server',
      nil,
      attempt
    )
    return
  end

  self:_probe(
    key,
    attempt,
    resolved,
    math.min(probe_timeout_ms, resolved.ready_timeout_ms),
    function(result)
      self:_guard(key, attempt, function()
        if self.disposed or self.in_flight[key] ~= attempt then
          return
        end
        if result.kind == "compatible" then
          self:_log("reusing compatible bingo instance " .. result.health.instance_id)
          self:_complete(key, nil, resolved.dap, attempt)
          return
        end
        if result.kind == "incompatible" then
          self:_complete(
            key,
            "cannot use bingo management endpoint "
              .. config.endpoint(resolved.management)
              .. ": "
              .. result.reason
              .. "; no server was started",
            nil,
            attempt
          )
          return
        end
        if result.kind == "transportError" then
          self:_complete(
            key,
            "cannot probe bingo management endpoint "
              .. config.endpoint(resolved.management)
              .. ": "
              .. tostring(result.error),
            nil,
            attempt
          )
          return
        end
        if result.kind ~= "absent" then
          self:_complete(key, "unexpected health result: " .. tostring(result.kind), nil, attempt)
          return
        end

        local child, spawn_error = self:_spawn(resolved)
        if child == nil then
          self:_complete(key, spawn_error, nil, attempt)
          return
        end
        local deadline = self.deps.uv.hrtime() / 1000000 + resolved.ready_timeout_ms
        self:_poll_ready(key, resolved, child, deadline, result, attempt)
      end)
    end
  )
end

function Manager:ensure(debug_config, callback)
  if self.disposed then
    self.deps.schedule(function()
      callback("bingo server manager is disposed")
    end)
    return
  end

  local ok, resolved = pcall(config.resolve, debug_config, self.options)
  if not ok then
    self.deps.schedule(function()
      callback(tostring(resolved))
    end)
    return
  end
  if resolved.mode == "connectOnly" then
    self.deps.schedule(function()
      if self.disposed then
        callback("bingo server startup was cancelled")
      else
        callback(nil, resolved.dap)
      end
    end)
    return
  end

  local key = config.endpoint(resolved.management) .. "|" .. config.endpoint(resolved.dap)
  if self.in_flight[key] ~= nil then
    self.in_flight[key][#self.in_flight[key] + 1] = callback
    return
  end
  local attempt = { callback }
  self.in_flight[key] = attempt
  self:_guard(key, attempt, function()
    self:_ensure_auto(key, resolved, attempt)
  end)
end

function Manager:dispose()
  if self.disposed then
    return
  end
  self.disposed = true
  for timer in pairs(self.timers) do
    if not timer:is_closing() then
      timer:stop()
      timer:close()
    end
  end
  self.timers = {}
  for key in pairs(self.in_flight) do
    self:_complete(key, "bingo server startup was cancelled")
  end
  for pid, handle in pairs(self.processes) do
    if not handle:is_closing() then
      handle:close()
    end
    self.processes[pid] = nil
  end
end

return M
