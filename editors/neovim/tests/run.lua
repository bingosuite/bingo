local script = arg[0]
local root = script:match("^(.*)/tests/run%.lua$") or "."
package.path = table.concat({
  root .. "/lua/?.lua",
  root .. "/lua/?/init.lua",
  package.path,
}, ";")

for _, path in ipairs({
  "lua/bingo/config.lua",
  "lua/bingo/health.lua",
  "lua/bingo/init.lua",
  "lua/bingo/server.lua",
  "lua/bingo/session.lua",
  "plugin/bingo.lua",
  "tests/run.lua",
}) do
  local chunk, parse_error = loadfile(root .. "/" .. path)
  if chunk == nil then
    error(parse_error)
  end
end

local failures = 0
local tests = 0

local function equal(actual, expected, message)
  if actual ~= expected then
    error(
      string.format(
        "%s: got %s, want %s",
        message or "values differ",
        tostring(actual),
        tostring(expected)
      )
    )
  end
end

local function test(name, callback)
  tests = tests + 1
  local ok, failure = pcall(callback)
  if ok then
    io.write("ok - " .. name .. "\n")
  else
    failures = failures + 1
    io.stderr:write("not ok - " .. name .. ": " .. tostring(failure) .. "\n")
  end
end

local config = require("bingo.config")
local health = require("bingo.health")
local server = require("bingo.server")
local session = require("bingo.session")

test("configuration defaults mirror the managed VS Code client", function()
  local options = config.normalize()
  local resolved = config.resolve({ request = "launch" }, options)
  equal(resolved.mode, "auto")
  equal(resolved.management.host, "127.0.0.1")
  equal(resolved.management.port, 6060)
  equal(resolved.dap.host, "127.0.0.1")
  equal(resolved.dap.port, 4711)
  equal(resolved.ready_timeout_ms, 5000)
  equal(resolved.idle_timeout_ms, 30000)
end)

test("debug configuration overrides lifecycle endpoints", function()
  local resolved = config.resolve({
    serverMode = "connectOnly",
    managementHost = "debug.internal",
    managementPort = 16060,
    dapHost = "debug.internal",
    dapPort = 14711,
    serverReadyTimeoutMs = 8000,
    managedIdleTimeoutMs = 45000,
  }, config.normalize())
  equal(resolved.mode, "connectOnly")
  equal(resolved.management.port, 16060)
  equal(resolved.dap.port, 14711)
  equal(resolved.ready_timeout_ms, 8000)
  equal(resolved.idle_timeout_ms, 45000)
end)

test("invalid endpoint configuration is rejected", function()
  local ok, failure = pcall(function()
    config.resolve({ dapPort = 0 }, config.normalize())
  end)
  equal(ok, false)
  if tostring(failure):find("dapPort", 1, true) == nil then
    error("error did not identify dapPort")
  end
end)

local valid_health = {
  service = "bingo",
  managementApiVersion = 1,
  wireProtocolVersion = "1.2",
  instanceId = "instance-1",
  dap = {
    enabled = true,
    address = "127.0.0.1:4711",
    sessionEventVersion = 1,
  },
}

test("compatible health is accepted", function()
  local result = health.validate(200, valid_health, {
    host = "127.0.0.1",
    port = 4711,
  })
  equal(result.kind, "compatible")
  equal(result.health.instance_id, "instance-1")
end)

test("incompatible DAP capability is rejected", function()
  local decoded = {
    service = "bingo",
    managementApiVersion = 1,
    wireProtocolVersion = "1.2",
    instanceId = "instance-1",
    dap = {
      enabled = true,
      address = "127.0.0.1:4711",
      sessionEventVersion = 0,
    },
  }
  local result = health.validate(200, decoded, {
    host = "127.0.0.1",
    port = 4711,
  })
  equal(result.kind, "incompatible")
  if result.reason:find("session event", 1, true) == nil then
    error("error did not identify the DAP session event")
  end
end)

test("HTTP health response parsing preserves the JSON body", function()
  local response, parse_error = health.parse_http_response(
    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"service\":\"bingo\"}"
  )
  equal(parse_error, nil)
  equal(response.status, 200)
  equal(response.body, '{"service":"bingo"}')
end)

test("bingo exposes a valid Neovim healthcheck", function()
  local real_vim = _G.vim
  local previous_preload = package.preload.dap
  local previous_loaded = package.loaded.dap
  local reports = {}
  _G.vim = setmetatable({
    health = {
      start = function(name)
        reports[#reports + 1] = "start:" .. name
      end,
      ok = function(message)
        reports[#reports + 1] = "ok:" .. message
      end,
      error = function(message)
        reports[#reports + 1] = "error:" .. message
      end,
      info = function(message)
        reports[#reports + 1] = "info:" .. message
      end,
    },
    fn = setmetatable({
      has = function()
        return 1
      end,
    }, { __index = real_vim.fn }),
  }, { __index = real_vim })
  package.preload.dap = function()
    return {}
  end
  package.loaded.dap = nil

  health.check()
  equal(reports[1], "start:bingo")
  equal(reports[2], "ok:Neovim 0.11.7 or newer is available")
  equal(reports[3], "ok:nvim-dap is available")

  package.preload.dap = previous_preload
  package.loaded.dap = previous_loaded
  _G.vim = real_vim
end)

test("health timeout prevents stale TCP callbacks from advancing", function()
  local timer_callback
  local connect_callback
  local writes = 0
  local result
  local timer_closed = false
  local tcp_closed = false
  local timer = {
    start = function(_, _, _, callback)
      timer_callback = callback
    end,
    stop = function()
    end,
    close = function()
      timer_closed = true
    end,
    is_closing = function()
      return timer_closed
    end,
  }
  local tcp = {
    connect = function(_, _, _, callback)
      connect_callback = callback
    end,
    write = function()
      writes = writes + 1
    end,
    close = function()
      tcp_closed = true
    end,
    is_closing = function()
      return tcp_closed
    end,
  }

  health.probe(
    { host = "127.0.0.1", port = 6060 },
    { host = "127.0.0.1", port = 4711 },
    100,
    function(value)
      result = value
    end,
    {
      uv = {
        new_tcp = function()
          return tcp
        end,
        new_timer = function()
          return timer
        end,
      },
      schedule = function(callback)
        callback()
      end,
      decode = function()
        return {}
      end,
    }
  )

  timer_callback()
  connect_callback(nil)
  equal(result.kind, "transportError")
  equal(writes, 0)
end)

test("disposing a server manager prevents a stale probe from spawning", function()
  local probe_callback
  local binary_checks = 0
  local results = {}
  local manager = server.new(config.normalize(), {
    uv = {
      os_uname = function()
        return { sysname = "Darwin", machine = "arm64" }
      end,
    },
    schedule = function(callback)
      callback()
    end,
    executable = function()
      binary_checks = binary_checks + 1
      return 0
    end,
    exepath = function()
      return ""
    end,
    mkdir = function()
    end,
    stdpath = function()
      return "/tmp"
    end,
    probe = function(_, _, _, callback)
      probe_callback = callback
    end,
  })

  manager:ensure({ request = "launch" }, function(error_message)
    results[#results + 1] = error_message
  end)
  manager:dispose()
  probe_callback({ kind = "absent" })

  equal(#results, 1)
  equal(results[1], "bingo server startup was cancelled")
  equal(binary_checks, 0)
end)

test("server startup failures release every coalesced waiter", function()
  local probe_callbacks = {}
  local callbacks = 0
  local callback_errors = {}
  local notifications = {}
  local manager = server.new(config.normalize({
    server = {
      binary = "/tmp/bingo",
      log_path = "/protected/bingo/server.log",
    },
  }), {
    uv = {
      os_uname = function()
        return { sysname = "Darwin", machine = "arm64" }
      end,
    },
    schedule = function(callback)
      callback()
    end,
    executable = function()
      return 1
    end,
    exepath = function(path)
      return path
    end,
    mkdir = function()
      error("Vim:E739: Cannot create directory")
    end,
    notify = function(message)
      notifications[#notifications + 1] = message
    end,
    stdpath = function()
      return "/tmp"
    end,
    probe = function(_, _, _, callback)
      probe_callbacks[#probe_callbacks + 1] = callback
    end,
  })

  manager:ensure({ request = "launch" }, function(error_message)
    callbacks = callbacks + 1
    callback_errors[#callback_errors + 1] = error_message
    error("first waiter failed")
  end)
  manager:ensure({ request = "launch" }, function(error_message)
    callbacks = callbacks + 1
    callback_errors[#callback_errors + 1] = error_message
  end)
  equal(#probe_callbacks, 1)
  probe_callbacks[1]({ kind = "absent" })
  equal(callbacks, 2)
  equal(next(manager.in_flight), nil)
  if notifications[1]:find("first waiter failed", 1, true) == nil then
    error("waiter callback failure was not reported")
  end
  if callback_errors[1]:find("cannot create bingo server log directory", 1, true) == nil then
    error("startup failure did not identify the log directory")
  end

  manager:ensure({ request = "launch" }, function(error_message)
    callbacks = callbacks + 1
    callback_errors[#callback_errors + 1] = error_message
  end)
  equal(#probe_callbacks, 2)
  probe_callbacks[2]({ kind = "absent" })
  equal(callbacks, 3)
  equal(next(manager.in_flight), nil)
end)

test("synchronous probe failures do not wedge future starts", function()
  local results = {}
  local probes = 0
  local manager = server.new(config.normalize(), {
    uv = {
      os_uname = function()
        return { sysname = "Darwin", machine = "arm64" }
      end,
    },
    schedule = function(callback)
      callback()
    end,
    executable = function()
      return 0
    end,
    exepath = function()
      return ""
    end,
    mkdir = function()
    end,
    stdpath = function()
      return "/tmp"
    end,
    probe = function()
      probes = probes + 1
      error("probe construction failed")
    end,
  })

  for _ = 1, 2 do
    manager:ensure({ request = "launch" }, function(error_message)
      results[#results + 1] = error_message
    end)
  end
  equal(probes, 2)
  equal(#results, 2)
  equal(next(manager.in_flight), nil)
  if results[1]:find("probe construction failed", 1, true) == nil then
    error("probe failure was not surfaced")
  end
end)

test("stale probes cannot complete a newer startup attempt", function()
  local probe_callbacks = {}
  local results = {}
  local manager = server.new(config.normalize(), {
    uv = {
      os_uname = function()
        return { sysname = "Darwin", machine = "arm64" }
      end,
    },
    schedule = function(callback)
      callback()
    end,
    executable = function()
      return 0
    end,
    exepath = function()
      return ""
    end,
    mkdir = function()
    end,
    stdpath = function()
      return "/tmp"
    end,
    probe = function(_, _, _, callback)
      probe_callbacks[#probe_callbacks + 1] = callback
      if #probe_callbacks == 1 then
        error("probe failed after arming a callback")
      end
    end,
  })

  manager:ensure({ request = "launch" }, function(error_message, endpoint)
    results[#results + 1] = { error = error_message, endpoint = endpoint }
  end)
  equal(#results, 1)
  manager:ensure({ request = "launch" }, function(error_message, endpoint)
    results[#results + 1] = { error = error_message, endpoint = endpoint }
  end)
  equal(#probe_callbacks, 2)

  probe_callbacks[1]({
    kind = "compatible",
    health = { instance_id = "stale" },
  })
  equal(#results, 1)
  probe_callbacks[2]({
    kind = "compatible",
    health = { instance_id = "current" },
  })
  equal(#results, 2)
  equal(results[2].error, nil)
  equal(results[2].endpoint.port, 4711)
end)

test("server manager starts and observes a compatible server", function()
  local probes = 0
  local spawn_request
  local closed_fd
  local unrefed = false
  local result_error
  local result_endpoint
  local handle = {
    unref = function()
      unrefed = true
    end,
    is_closing = function()
      return false
    end,
    close = function()
    end,
  }
  local manager = server.new(config.normalize({
    server = {
      binary = "/tmp/bingo",
      log_path = "/tmp/bingo.log",
      ready_timeout_ms = 100,
      idle_timeout_ms = 250,
    },
  }), {
    uv = {
      os_uname = function()
        return { sysname = "Darwin", machine = "arm64" }
      end,
      fs_open = function()
        return 17
      end,
      fs_close = function(fd)
        closed_fd = fd
      end,
      spawn = function(binary, options)
        spawn_request = { binary = binary, options = options }
        return handle, 4242
      end,
      hrtime = function()
        return 0
      end,
    },
    schedule = function(callback)
      callback()
    end,
    executable = function()
      return 1
    end,
    exepath = function(path)
      return path
    end,
    mkdir = function()
    end,
    stdpath = function()
      return "/tmp"
    end,
    probe = function(_, _, _, callback)
      probes = probes + 1
      if probes == 1 then
        callback({ kind = "absent" })
      else
        callback({
          kind = "compatible",
          health = { instance_id = "instance-ready" },
        })
      end
    end,
  })

  manager:ensure({ request = "launch" }, function(error_message, endpoint)
    result_error = error_message
    result_endpoint = endpoint
  end)

  equal(result_error, nil)
  equal(result_endpoint.host, "127.0.0.1")
  equal(result_endpoint.port, 4711)
  equal(probes, 2)
  equal(spawn_request.binary, "/tmp/bingo")
  equal(spawn_request.options.detached, true)
  equal(spawn_request.options.args[1], "-addr")
  equal(spawn_request.options.args[2], "127.0.0.1:6060")
  equal(spawn_request.options.args[5], "-idle-timeout")
  equal(spawn_request.options.args[6], "250ms")
  equal(spawn_request.options.stdio[2], 17)
  equal(spawn_request.options.stdio[3], 17)
  equal(closed_fd, 17)
  equal(unrefed, true)
end)

test("managed session announcements are strict", function()
  local announcement, decode_error = session.decode({
    version = 1,
    sessionId = "session-123",
  })
  equal(decode_error, nil)
  equal(announcement.session_id, "session-123")

  local invalid, invalid_error = session.decode({
    version = 1,
    sessionId = "../bad",
  })
  equal(invalid, nil)
  if invalid_error == nil then
    error("invalid session id was accepted")
  end
end)

test("nvim-dap registrations and prompts follow the plugin lifecycle", function()
  local real_vim = _G.vim
  local notifications = {}
  local autocmd
  local input_value = ""
  local input_defaults = {}
  local runs = {}
  _G.vim = setmetatable({
    notify = function(message, level)
      notifications[#notifications + 1] = { message = message, level = level }
    end,
    schedule = function(callback)
      callback()
    end,
    api = setmetatable({
      nvim_exec_autocmds = function(_, options)
        autocmd = options
      end,
    }, { __index = real_vim.api }),
    fn = setmetatable({
      input = function(_, default)
        input_defaults[#input_defaults + 1] = default
        return input_value
      end,
    }, { __index = real_vim.fn }),
  }, { __index = real_vim })

  local previous_adapter = function()
  end
  local user_configuration = {
    name = "user configuration",
    type = "bingo",
    request = "launch",
  }
  local dap = {
    ABORT = {},
    adapters = { bingo = previous_adapter },
    configurations = { go = { user_configuration } },
    listeners = {
      before = {},
      after = {},
      on_session = {},
    },
    run = function(value)
      runs[#runs + 1] = value
    end,
  }
  package.preload.dap = function()
    return dap
  end
  package.loaded.dap = nil
  package.loaded["bingo"] = nil
  package.loaded["bingo.init"] = nil

  local bingo = require("bingo")
  bingo.setup()
  equal(type(dap.adapters.bingo), "function")
  equal(#dap.configurations.go, 4)
  local first_adapter = dap.adapters.bingo

  local adapter
  dap.adapters.bingo(function(value)
    adapter = value
  end, {
    request = "launch",
    serverMode = "connectOnly",
  })
  equal(adapter.type, "server")
  equal(adapter.host, "127.0.0.1")
  equal(adapter.port, 4711)

  local configurations = {}
  for _, item in ipairs(dap.configurations.go) do
    configurations[item.name] = item
  end
  equal(configurations["bingo: Launch binary"].program(), dap.ABORT)
  equal(configurations["bingo: Attach to process"].pid(), dap.ABORT)
  equal(configurations["bingo: Attach to process"].binaryPath(), "")
  equal(input_defaults[#input_defaults], "")
  equal(configurations["bingo: Join session"].session(), dap.ABORT)

  bingo.attach(123)
  equal(#runs, 1)
  equal(runs[1].binaryPath, "")

  local event_listener = dap.listeners.before["event_bingo/session/v1"].bingo
  local dap_session = {}
  event_listener(dap_session, {
    version = 1,
    sessionId = "session-456",
  })
  equal(bingo.session_id(), "session-456")
  equal(autocmd.pattern, "BingoSession")
  equal(autocmd.data.session_id, "session-456")

  local managed_session = {
    config = { type = "bingo" },
    on_close = {},
  }
  dap.listeners.on_session.bingo(nil, managed_session)
  equal(type(managed_session.on_close.bingo), "function")
  managed_session.on_close.bingo(dap_session)
  equal(bingo.session_id(), nil)

  local setup_ok = pcall(function()
    bingo.setup({ server = "invalid" })
  end)
  equal(setup_ok, false)
  equal(dap.adapters.bingo, first_adapter)

  bingo.setup()
  equal(#dap.configurations.go, 4)
  equal(dap.configurations.go[1], user_configuration)

  bingo.setup({ configurations = false })
  equal(#dap.configurations.go, 1)
  equal(dap.configurations.go[1], user_configuration)
  local second_adapter = dap.adapters.bingo
  equal(type(second_adapter), "function")

  local stale_ok = pcall(first_adapter, function()
    error("disposed adapter unexpectedly resolved")
  end, {
    request = "launch",
    serverMode = "connectOnly",
  })
  equal(stale_ok, true)
  if notifications[#notifications].message:find("disposed", 1, true) == nil then
    error("stale adapter did not report its disposed manager")
  end

  bingo.dispose()
  equal(dap.adapters.bingo, previous_adapter)
  equal(#dap.configurations.go, 1)
  equal(dap.listeners.before["event_bingo/session/v1"].bingo, nil)
  equal(dap.listeners.before.event_terminated.bingo, nil)
  equal(dap.listeners.before.event_exited.bingo, nil)
  equal(dap.listeners.on_session.bingo, nil)

  local disposed_ok = pcall(second_adapter, function()
    error("disposed adapter unexpectedly resolved")
  end, {
    request = "launch",
    serverMode = "connectOnly",
  })
  equal(disposed_ok, true)
  if notifications[#notifications].message:find("disposed", 1, true) == nil then
    error("disposed adapter did not report its state")
  end

  package.preload.dap = nil
  package.loaded.dap = nil
  _G.vim = real_vim
end)

if failures > 0 then
  io.stderr:write(string.format("%d of %d tests failed\n", failures, tests))
  os.exit(1)
end
io.write(string.format("%d tests passed\n", tests))
