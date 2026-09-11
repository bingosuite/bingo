return function(test, equal, T)
  local function setup(options)
    local real = vim
    local c = T.clock()
    local f = { notifications = {}, events = {}, runs = {}, managers = {}, inputs = {}, commands = {} }
    local dap = {
      ABORT = {}, adapters = {}, configurations = { go = {} },
      listeners = { before = {}, after = {}, on_session = {} },
      run = function(value) f.runs[#f.runs + 1] = value end,
    }
    _G.vim = setmetatable({
      schedule = c.schedule,
      notify = function(message, level) f.notifications[#f.notifications + 1] = { message, level } end,
      g = {},
      fn = setmetatable({
        has = function() return 1 end,
        input = function() return table.remove(f.inputs, 1) or "" end,
      }, { __index = real.fn }),
      api = setmetatable({
        nvim_exec_autocmds = function(name, value) f.events[#f.events + 1] = { name, value } end,
        nvim_create_user_command = function(name, callback, settings)
          assert(f.commands[name] == nil, "duplicate command " .. name)
          f.commands[name] = { callback = callback, settings = settings }
        end,
      }, { __index = real.api }),
    }, { __index = real })
    package.loaded.dap = dap
    package.loaded["bingo.server"] = {
      new = function()
        local manager = { requests = {}, disposals = 0 }
        function manager:ensure(request, callback)
          self.requests[#self.requests + 1] = request
          c.schedule(function()
            if self.disposals > 0 then callback("manager disposed")
            else callback(self.error, { host = "127.0.0.1", port = 4711 }) end
          end)
        end
        function manager:dispose() self.disposals = self.disposals + 1 end
        f.managers[#f.managers + 1] = manager
        return manager
      end,
    }
    package.loaded.bingo = nil
    local bingo = require("bingo")
    f.previous_adapter = function() end
    dap.adapters.bingo = f.previous_adapter
    bingo.setup(options)
    T.defer(function() bingo.dispose(); c.flush() end)
    f.bingo, f.dap, f.clock = bingo, dap, c
    function f.new_session(kind)
      local s = { config = { type = kind or "bingo" }, on_close = {} }
      dap.listeners.on_session.bingo(nil, s)
      return s
    end
    function f.announce(s, id)
      dap.listeners.before["event_bingo/session/v1"].bingo(s, { version = 1, sessionId = id })
    end
    return f
  end

  test("adapter resolves TCP with exact timeout settings and no Delve adapter", function()
    local f = setup()
    local results = {}
    f.dap.adapters.bingo(function(value) results[#results + 1] = value end,
      { request = "launch", program = "/tmp/target" })
    equal(#results, 0)
    f.clock.flush()
    equal(#results, 1)
    equal(results[1].type, "server")
    equal(results[1].port, 4711)
    equal(results[1].host, "127.0.0.1")
    equal(results[1].options.initialize_timeout_sec, 10)
    equal(results[1].options.disconnect_timeout_sec, 5)
    equal(f.dap.adapters.go, nil)
  end)
  test("invalid requests never reach the manager or adapter callback", function()
    local f = setup()
    for _, request in ipairs({
      {}, { request = "launch" }, { request = "attach" },
      { request = "attach", pid = 1, session = "s" },
      { request = "attach", session = "../s" },
      { request = "launch", program = "/x", stopOnEntry = "true" },
    }) do
      f.dap.adapters.bingo(function() error("invalid adapter resolved") end, request)
    end
    f.clock.flush()
    equal(#f.managers[1].requests, 0)
    equal(#f.notifications, 6)
  end)
  test("failed manager reports actionable error without resolving adapter", function()
    local f = setup()
    f.managers[1].error = "occupied management port"
    f.dap.adapters.bingo(function() error("failed adapter resolved") end,
      { request = "attach", session = "session" })
    f.clock.flush()
    equal(#f.notifications, 1)
    T.contains(f.notifications[1][1], "occupied management port")
  end)
  test("throwing adapter callback is reported, not an unhandled scheduled exception", function()
    local f = setup()
    f.dap.adapters.bingo(function() error("adapter consumer failed") end,
      { request = "attach", pid = 1 })
    f.clock.flush()
    equal(#f.notifications, 1)
    T.contains(f.notifications[1][1], "adapter consumer failed")
  end)
  test("repeated setup restores only owned adapter/configuration/listener registrations", function()
    local f = setup()
    local user = { type = "bingo", name = "user", request = "launch", program = "/x" }
    table.insert(f.dap.configurations.go, user)
    for _ = 1, 50 do f.bingo.setup() end
    equal(#f.dap.configurations.go, 4)
    for i = 1, #f.managers - 1 do equal(f.managers[i].disposals, 1) end
    local foreign_adapter, foreign_listener = function() end, function() end
    f.dap.adapters.bingo = foreign_adapter
    f.dap.listeners.before.event_terminated.bingo = foreign_listener
    f.bingo.dispose()
    equal(f.dap.adapters.bingo, foreign_adapter)
    equal(f.dap.listeners.before.event_terminated.bingo, foreign_listener)
    equal(#f.dap.configurations.go, 1)
    equal(f.dap.configurations.go[1], user)
  end)
  test("invalid setup preserves a working registration and active session", function()
    local f = setup()
    local s = f.new_session()
    f.announce(s, "alive")
    local adapter = f.dap.adapters.bingo
    T.raises(function() f.bingo.setup({ server = false }) end, "server options")
    equal(f.dap.adapters.bingo, adapter)
    equal(f.bingo.session_id(), "alive")
    equal(f.managers[1].disposals, 0)
  end)
  test("unsupported Neovim rejects setup without replacing working registrations", function()
    local f = setup()
    local adapter = f.dap.adapters.bingo
    vim.fn.has = function() return 0 end
    T.raises(function() f.bingo.setup() end, "Neovim 0.11.7")
    equal(f.dap.adapters.bingo, adapter)
    equal(#f.managers, 1)
    equal(f.managers[1].disposals, 0)
  end)
  for _, available in ipairs({ true, false }) do
    test("checkhealth reports Neovim and nvim-dap dependency availability: " .. tostring(available), function()
      local f = setup()
      local statuses = {}
      vim.health = {}
      for _, kind in ipairs({ "start", "ok", "error", "info" }) do
        vim.health[kind] = function(message)
          statuses[#statuses + 1] = { kind = kind, message = message }
        end
      end
      if not available then
        vim.fn.has = function() return 0 end
        package.loaded.dap = nil
        package.preload.dap = function() error("injected missing nvim-dap") end
      end
      require("bingo.health").check()
      equal(#statuses, 4)
      equal(statuses[2].kind, available and "ok" or "error")
      equal(statuses[3].kind, available and "ok" or "error")
      T.contains(statuses[2].message, "Neovim")
      T.contains(statuses[3].message, "nvim-dap")
      equal(#f.managers[1].requests, 0)
    end)
  end
  test("session events ignore foreign, unregistered, terminated, and stale generations", function()
    local f = setup()
    local foreign = f.new_session("go")
    local s = f.new_session()
    local old_event = f.dap.listeners.before["event_bingo/session/v1"].bingo
    f.announce(foreign, "foreign")
    f.announce({}, "unregistered")
    equal(#f.events, 0)
    f.announce(s, "owned")
    equal(f.bingo.session_id(), "owned")
    f.dap.listeners.before.event_terminated.bingo(s)
    f.announce(s, "late")
    equal(f.bingo.session_id(), nil)
    equal(s.on_close.bingo, nil)
    f.bingo.setup()
    old_event(s, { version = 1, sessionId = "old-generation" })
    equal(#f.events, 1)
    equal(f.bingo.session_id(), nil)
  end)
  test("announcements emit once and reject conflicting IDs without overwriting identity", function()
    local f = setup({ notify_session = false })
    local s = f.new_session()
    f.announce(s, "one"); f.announce(s, "one"); f.announce(s, "two")
    equal(f.bingo.session_id(), "one")
    equal(#f.events, 1)
    equal(f.events[1][1], "User")
    equal(f.events[1][2].pattern, "BingoSession")
    equal(f.events[1][2].modeline, false)
    equal(f.events[1][2].data.session_id, "one")
    equal(#f.notifications, 1)
    T.contains(f.notifications[1][1], "conflicting")
  end)
  test("invalid session body is warned and cannot change the current model", function()
    local f = setup()
    local s = f.new_session()
    f.dap.listeners.before["event_bingo/session/v1"].bingo(s, { version = 2, sessionId = "a" })
    equal(f.bingo.session_id(), nil)
    equal(#f.events, 0)
    T.contains(f.notifications[1][1], "invalid bingo session event")
  end)
  test("multisession close falls back to another live identity and clears the last", function()
    local f = setup()
    local a, b = f.new_session(), f.new_session()
    f.announce(a, "a"); f.announce(b, "b")
    equal(f.bingo.session_id(), "b")
    b.on_close.bingo(b)
    f.clock.flush()
    equal(f.bingo.session_id(), "a")
    f.dap.listeners.before.event_exited.bingo(a)
    equal(f.bingo.session_id(), nil)
    equal(a.on_close.bingo, nil)
    equal(b.on_close.bingo, nil)
  end)
  test("queued old close cannot erase a session re-registered after setup", function()
    local f = setup()
    local s = f.new_session()
    f.announce(s, "before")
    s.on_close.bingo(s)
    f.bingo.setup()
    equal(s.on_close.bingo, nil)
    f.dap.listeners.on_session.bingo(nil, s)
    f.announce(s, "after")
    f.clock.flush()
    equal(f.bingo.session_id(), "after")
  end)
  test("existing on_close ownership is preserved and callback cleanup survives its error", function()
    local f = setup()
    local calls = 0
    local prior = function() calls = calls + 1; error("user hook failed") end
    local s = { config = { type = "bingo" }, on_close = { bingo = prior } }
    f.dap.listeners.on_session.bingo(nil, s)
    f.dap.listeners.on_session.bingo(nil, s)
    f.announce(s, "s")
    T.raises(function() s.on_close.bingo(s) end, "user hook failed")
    f.clock.flush()
    equal(calls, 1)
    equal(f.bingo.session_id(), nil)
    equal(s.on_close.bingo, prior)
  end)
  test("large session churn releases each on_close hook and weakly owns session objects", function()
    local f = setup({ notify_session = false })
    local weak = setmetatable({}, { __mode = "v" })
    for i = 1, 1000 do
      local s = f.new_session()
      weak[i] = s
      f.announce(s, "session-" .. i)
      s.on_close.bingo(s)
      f.clock.flush()
      equal(s.on_close.bingo, nil)
    end
    collectgarbage("collect")
    equal(next(weak), nil)
    equal(f.bingo.session_id(), nil)
    equal(#f.events, 1000)
  end)
  test("disposal restores active session hooks and fences retained listeners", function()
    local f = setup()
    local s = f.new_session()
    local hook = s.on_close.bingo
    local listener = f.dap.listeners.before["event_bingo/session/v1"].bingo
    f.announce(s, "s")
    f.bingo.dispose()
    equal(s.on_close.bingo, nil)
    hook(s); f.clock.flush()
    listener(s, { version = 1, sessionId = "late" })
    equal(f.bingo.session_id(), nil)
    equal(#f.events, 1)
    equal(f.dap.adapters.bingo, f.previous_adapter)
  end)
  test("launch/attach/join helpers validate before invoking dap.run", function()
    local f = setup()
    f.bingo.launch("/tmp/path with spaces")
    f.bingo.attach(123, "")
    f.bingo.join("session-1")
    equal(#f.runs, 3)
    equal(f.runs[1].program, "/tmp/path with spaces")
    equal(f.runs[2].pid, 123)
    equal(f.runs[2].binaryPath, "")
    equal(f.runs[3].session, "session-1")
    equal(f.runs[3].pid, nil)
    for _, pid in ipairs({ -1, 1.5, math.huge, 0 / 0, 2147483648 }) do f.bingo.attach(pid, "") end
    f.bingo.attach(1, false)
    f.bingo.launch("a\0b")
    f.bingo.join("../bad")
    equal(#f.runs, 3)
  end)
  test("cancelled prompts abort default configurations without running anything", function()
    local f = setup()
    local configurations = f.dap.configurations.go
    equal(configurations[1].program(), f.dap.ABORT)
    equal(configurations[2].pid(), f.dap.ABORT)
    equal(configurations[3].session(), f.dap.ABORT)
    f.bingo.launch(); f.bingo.join(); f.bingo.attach()
    equal(#f.runs, 0)
  end)
  test("plugin commands register once and reject malformed attach arguments without prompting", function()
    local f = setup()
    assert(loadfile(T.root .. "/plugin/bingo.lua"))()
    assert(loadfile(T.root .. "/plugin/bingo.lua"))()
    local count = 0
    for _ in pairs(f.commands) do count = count + 1 end
    equal(count, 4)
    f.commands.BingoAttach.callback({ fargs = { "invalid" } })
    f.commands.BingoAttach.callback({ fargs = { "1", "/x", "extra" } })
    equal(#f.runs, 0)
    equal(#f.notifications, 2)
    f.commands.BingoLaunch.callback({ args = "/tmp/a b" })
    f.commands.BingoJoin.callback({ args = "session" })
    f.commands.BingoAttach.callback({ fargs = { "123", "/tmp/x" } })
    equal(#f.runs, 3)
    f.commands.BingoSession.callback()
    T.contains(f.notifications[#f.notifications][1], "no active")
  end)
end
