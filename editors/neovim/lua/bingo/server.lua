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
  return dirname(dirname(dirname(source)))
end

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

function Manager:_complete(key, error_message, endpoint)
  local waiters = self.in_flight[key]
  if waiters == nil then
    return
  end
  self.in_flight[key] = nil
  for _, callback in ipairs(waiters) do
    callback(error_message, endpoint)
  end
end

function Manager:_later(delay_ms, callback)
  local timer = self.deps.uv.new_timer()
  self.timers[timer] = true
  timer:start(delay_ms, 0, function()
    timer:stop()
    timer:close()
    self.timers[timer] = nil
    self.deps.schedule(callback)
  end)
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

  local bundled = vim.fs.joinpath(source_root(), "bin", "bingo")
  if self.deps.executable(bundled) == 1 then
    return bundled
  end
  local path_binary = self.deps.exepath("bingo")
  if path_binary ~= "" then
    return path_binary
  end
  return nil,
    "no bingo server binary found; run `just neovim-prepare`, put bingo on PATH, or set server.binary"
end

function Manager:_log_path(resolved)
  if resolved.log_path ~= nil then
    return resolved.log_path
  end
  return vim.fs.joinpath(self.deps.stdpath("state"), "bingo", "server.log")
end

function Manager:_spawn(resolved)
  local binary, binary_error = self:_resolve_binary(resolved)
  if binary == nil then
    return nil, binary_error
  end

  local log_path = self:_log_path(resolved)
  self.deps.mkdir(vim.fs.dirname(log_path))
  local log_fd, open_error = self.deps.uv.fs_open(log_path, "a", 420)
  if log_fd == nil then
    return nil, "cannot open bingo server log " .. log_path .. ": " .. tostring(open_error)
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
  handle, pid = self.deps.uv.spawn(binary, {
    args = args,
    detached = true,
    stdio = { nil, log_fd, log_fd },
  }, function(code, signal)
    if handle ~= nil and not handle:is_closing() then
      handle:close()
    end
    self.deps.schedule(function()
      self.processes[pid] = nil
      self:_log(
        string.format(
          "managed bingo server %s exited with code %d signal %d",
          tostring(pid),
          code,
          signal
        )
      )
    end)
  end)
  self.deps.uv.fs_close(log_fd)
  if handle == nil then
    return nil, "cannot start bingo server: " .. tostring(pid) .. "; logs: " .. log_path
  end

  handle:unref()
  self.processes[pid] = handle
  self:_log("starting managed bingo server; logs: " .. log_path)
  return { pid = pid, log_path = log_path }
end

function Manager:_poll_ready(key, resolved, child, deadline_ms, last_result)
  if self.disposed or self.in_flight[key] == nil then
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
      )
    )
    return
  end

  self.deps.probe(
    resolved.management,
    resolved.dap,
    math.min(probe_timeout_ms, math.max(1, math.floor(remaining))),
    function(result)
      if self.disposed or self.in_flight[key] == nil then
        return
      end
      if result.kind == "compatible" then
        self:_log("reusing compatible bingo instance " .. result.health.instance_id)
        self:_complete(key, nil, resolved.dap)
        return
      end
      if result.kind == "incompatible" then
        self:_complete(
          key,
          "cannot use bingo management endpoint "
            .. config.endpoint(resolved.management)
            .. ": "
            .. result.reason
        )
        return
      end
      local poll_remaining = deadline_ms - self.deps.uv.hrtime() / 1000000
      self:_later(math.max(1, math.min(poll_interval_ms, poll_remaining)), function()
        self:_poll_ready(key, resolved, child, deadline_ms, result)
      end)
    end
  )
end

function Manager:_ensure_auto(key, resolved)
  if resolved.management.host ~= "127.0.0.1" or resolved.dap.host ~= "127.0.0.1" then
    self:_complete(
      key,
      'serverMode "auto" requires managementHost and dapHost to be 127.0.0.1; use "connectOnly" for remote or custom endpoints'
    )
    return
  end
  if not supported_platform(self.deps.uv) then
    self:_complete(
      key,
      'bingo server autostart supports only linux/amd64 and darwin/arm64; use serverMode "connectOnly" with an existing server'
    )
    return
  end

  self.deps.probe(
    resolved.management,
    resolved.dap,
    math.min(probe_timeout_ms, resolved.ready_timeout_ms),
    function(result)
      if self.disposed or self.in_flight[key] == nil then
        return
      end
      if result.kind == "compatible" then
        self:_log("reusing compatible bingo instance " .. result.health.instance_id)
        self:_complete(key, nil, resolved.dap)
        return
      end
      if result.kind == "incompatible" then
        self:_complete(
          key,
          "cannot use bingo management endpoint "
            .. config.endpoint(resolved.management)
            .. ": "
            .. result.reason
            .. "; no server was started"
        )
        return
      end
      if result.kind == "transportError" then
        self:_complete(
          key,
          "cannot probe bingo management endpoint "
            .. config.endpoint(resolved.management)
            .. ": "
            .. tostring(result.error)
        )
        return
      end

      local child, spawn_error = self:_spawn(resolved)
      if child == nil then
        self:_complete(key, spawn_error)
        return
      end
      local deadline = self.deps.uv.hrtime() / 1000000 + resolved.ready_timeout_ms
      self:_poll_ready(key, resolved, child, deadline, result)
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
      callback(nil, resolved.dap)
    end)
    return
  end

  local key = config.endpoint(resolved.management) .. "|" .. config.endpoint(resolved.dap)
  if self.in_flight[key] ~= nil then
    self.in_flight[key][#self.in_flight[key] + 1] = callback
    return
  end
  self.in_flight[key] = { callback }
  self:_ensure_auto(key, resolved)
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
end

return M
