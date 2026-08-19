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

test("nvim-dap adapter and session listener are registered", function()
  local real_vim = _G.vim
  local notifications = {}
  local autocmd
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
  }, { __index = real_vim })

  local dap = {
    adapters = {},
    configurations = {},
    listeners = {
      before = {},
      after = {},
      on_session = {},
    },
    run = function()
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
  equal(#dap.configurations.go, 3)

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
  equal(#notifications, 1)
  bingo.dispose()
  package.preload.dap = nil
  package.loaded.dap = nil
  _G.vim = real_vim
end)

if failures > 0 then
  io.stderr:write(string.format("%d of %d tests failed\n", failures, tests))
  os.exit(1)
end
io.write(string.format("%d tests passed\n", tests))
