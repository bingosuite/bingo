return function(test, equal, T)
  local function setup(options, before_setup)
    local real = vim
    local c = T.clock()
    local f = {
      notifications = {}, events = {}, runs = {}, managers = {}, inputs = {}, commands = {},
      prompts = 0, buffer_name = "", cwd = real.fn.getcwd(),
      info = {
        mode = "auto", platform_supported = true, platform = "Linux",
        binary = "/prepared bingo", dap = "127.0.0.1:4711", management = "127.0.0.1:6060",
      },
    }
    local dap = {
      ABORT = {}, adapters = {}, configurations = { go = {} },
      listeners = { before = {}, after = {}, on_session = {} },
      run = function(value) f.runs[#f.runs + 1] = value end,
    }
    _G.vim = setmetatable({
      schedule = c.schedule,
      notify = function(message, level) f.notifications[#f.notifications + 1] = { message, level } end,
      g = {},
      bo = { buftype = "" },
      fn = setmetatable({
        has = function() return 1 end,
        getcwd = function() return f.cwd end,
        input = function()
          f.prompts = f.prompts + 1
          local value = table.remove(f.inputs, 1)
          if type(value) == "function" then return value() end
          return value or ""
        end,
      }, { __index = real.fn }),
      api = setmetatable({
        nvim_exec_autocmds = function(name, value) f.events[#f.events + 1] = { name, value } end,
        nvim_buf_get_name = function() return f.buffer_name end,
        nvim_create_user_command = function(name, callback, settings)
          assert(f.commands[name] == nil, "duplicate command " .. name)
          f.commands[name] = { callback = callback, settings = settings }
        end,
      }, { __index = real.api }),
    }, { __index = real })
    package.loaded.dap = dap
    package.loaded["bingo.server"] = {
      inspect = function() return f.info end,
      new = function()
        local manager = { requests = {}, callbacks = {}, disposals = 0 }
        function manager:ensure(request, callback)
          self.requests[#self.requests + 1] = request
          self.callbacks[#self.callbacks + 1] = callback
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
    if before_setup then before_setup(dap) end
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
    function f.configuration(name)
      for _, value in ipairs(dap.configurations.go) do
        if value.name == "bingo: " .. name then return value end
      end
      error("missing configuration " .. name)
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
    equal(#f.dap.configurations.go, 5)
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
      for _, kind in ipairs({ "start", "ok", "error", "info", "warn" }) do
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
      assert(#statuses >= 4)
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
    equal(f.runs[1].mode, "exec")
    equal(f.runs[1].stopOnEntry, true)
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
  test("Debug Go package is the primary default and needs no executable prompt", function()
    local f = setup()
    f.buffer_name = vim.fn.fnamemodify(T.root .. "/tests/fixtures/smoke/main.go", ":p")
    local directory = vim.fs.dirname(f.buffer_name)
    local configuration = f.configuration("Debug Go package")
    equal(f.dap.configurations.go[1], configuration)
    equal(configuration.mode, "debug")
    equal(configuration.stopOnEntry, false)
    equal(configuration.program(), directory)
    f.bingo.debug()
    equal(#f.runs, 1)
    equal(f.runs[1].program, directory)
    equal(f.runs[1].cwd, directory)
    equal(f.runs[1].mode, "debug")
    equal(f.runs[1].stopOnEntry, false)
    equal(f.prompts, 0)
    equal(#f.managers[1].requests, 0)
  end)
  test("source startup chooses the editor cwd outside a regular Go buffer", function()
    local f = setup()
    f.cwd = T.tempdir()
    for _, value in ipairs({ "", "/tmp/README.md", "/tmp/not-go.go.txt" }) do
      f.buffer_name = value
      f.bingo.debug()
      equal(f.runs[#f.runs].program, f.cwd)
    end
    f.buffer_name, vim.bo.buftype = "/tmp/terminal.go", "terminal"
    f.bingo.debug()
    equal(f.runs[4].program, f.cwd)
    equal(f.prompts, 0)
  end)
  test("invalid source directories and explicit false arguments never prompt or call dap.run", function()
    local f = setup()
    local root = T.tempdir()
    assert(vim.fn.writefile({ "package main" }, root .. "/main.go") == 0)
    for _, value in ipairs({ root .. "/missing", root .. "/main.go", false, "", " ", "a\0b" }) do
      f.bingo.debug(value)
    end
    f.bingo.launch(false); f.bingo.attach(false); f.bingo.join(false)
    equal(#f.runs, 0)
    equal(#f.managers[1].requests, 0)
    equal(f.prompts, 0)
    equal(#f.notifications, 9)
  end)
  test("out-of-range PID is rejected before the optional executable prompt", function()
    local f = setup()
    f.bingo.attach(2147483648)
    equal(f.prompts, 0)
    equal(#f.runs, 0)
    T.contains(f.notifications[1][1], "positive process ID")
  end)
  test("invalid mode or cwd is rejected before the server manager and DAP transport", function()
    local f = setup()
    local root = T.tempdir()
    for _, request in ipairs({
      { request = "launch", program = root, mode = "test" },
      { request = "launch", program = root, mode = "debug", cwd = root .. "/missing" },
      { request = "launch", program = "/x", mode = "exec", cwd = false },
      { request = "launch", program = root, mode = "debug", dapPort = 6060 },
    }) do
      f.dap.adapters.bingo(function() error("invalid adapter resolved") end, request)
    end
    f.clock.flush()
    equal(#f.managers[1].requests, 0)
    equal(#f.notifications, 4)
  end)
  test("evaluated local DAP configurations send normalized source and cwd to the server", function()
    local f = setup()
    f.cwd = T.tempdir()
    assert(vim.fn.mkdir(f.cwd .. "/package") == 1)
    local evaluated = { request = "launch", mode = "debug", program = "package" }
    local resolved, timeout = 0, nil
    f.dap.adapters.bingo(function(adapter)
      resolved = resolved + 1
      timeout = adapter.options.initialize_timeout_sec
    end, evaluated)
    equal(evaluated.program, f.cwd .. "/package")
    equal(evaluated.cwd, f.cwd .. "/package")
    f.clock.flush()
    equal(resolved, 1)
    equal(timeout, 130)
    equal(f.managers[1].requests[1], evaluated)
  end)
  test("connectOnly debug helper preserves server-only source paths and never prompts", function()
    local f = setup({ server = { mode = "connectOnly", dap_host = "debug.internal" } })
    f.bingo.debug("/server only/cmd/app")
    equal(#f.runs, 1)
    equal(f.runs[1].program, "/server only/cmd/app")
    equal(f.runs[1].cwd, nil)
    equal(f.prompts, 0)
  end)
  test("user configurations including an overridden source default survive setup and dispose", function()
    local user = { name = "bingo: Debug Go package", type = "bingo", request = "launch", mode = "exec", program = "/user binary" }
    local f = setup(nil, function(dap) dap.configurations.go = { user } end)
    for _ = 1, 3 do f.bingo.setup() end
    equal(#f.dap.configurations.go, 4)
    equal(f.configuration("Debug Go package"), user)
    equal(user.mode, "exec")
    f.bingo.dispose()
    equal(#f.dap.configurations.go, 1)
    equal(f.dap.configurations.go[1], user)
  end)
  test("repeated setup restores pre-existing listener ownership when disposed", function()
    local prior = function() end
    local f = setup(nil, function(dap)
      dap.listeners.on_session.bingo = prior
      for _, name in ipairs({ "event_bingo/session/v1", "event_terminated", "event_exited" }) do
        dap.listeners.before[name] = { bingo = prior }
      end
    end)
    f.bingo.setup(); f.bingo.setup(); f.bingo.dispose()
    equal(f.dap.listeners.on_session.bingo, prior)
    for _, name in ipairs({ "event_bingo/session/v1", "event_terminated", "event_exited" }) do
      equal(f.dap.listeners.before[name].bingo, prior)
    end
  end)
  test("old adapter closures and queued success or failure cannot affect a replacement setup", function()
    local f = setup()
    local old_adapter = f.dap.adapters.bingo
    old_adapter(function() error("stale adapter resolved") end,
      { request = "launch", mode = "exec", program = "/binary" })
    local old_callback = f.managers[1].callbacks[1]
    f.bingo.setup()
    old_adapter(function() error("disposed adapter resolved") end, {})
    old_callback(nil, { host = "127.0.0.1", port = 4711 })
    old_callback("stale startup failed")
    f.clock.flush()
    equal(#f.managers[1].requests, 1)
    equal(#f.managers[2].requests, 0)
    equal(#f.notifications, 0)
  end)
  test("Ctrl-C aborts required and optional prompts without launching or reporting failure", function()
    local f = setup()
    local function interrupt() error("Vim:Interrupt") end
    f.inputs = { interrupt, interrupt, interrupt, interrupt }
    equal(f.configuration("Launch binary").program(), f.dap.ABORT)
    equal(f.configuration("Attach to process").pid(), f.dap.ABORT)
    equal(f.configuration("Attach to process").binaryPath(), f.dap.ABORT)
    equal(f.configuration("Join session").session(), f.dap.ABORT)
    f.inputs = { interrupt, interrupt, interrupt, interrupt }
    f.bingo.launch(); f.bingo.attach(); f.bingo.attach(123); f.bingo.join()
    equal(#f.runs, 0)
    equal(#f.notifications, 0)
  end)
  test("unexpected prompt errors are reported once rather than treated as cancellation", function()
    local f = setup()
    f.inputs = { function() error("input unavailable") end }
    f.bingo.attach(123)
    equal(#f.runs, 0)
    equal(#f.notifications, 1)
    T.contains(f.notifications[1][1], "prompt failed")
    T.contains(f.notifications[1][1], "input unavailable")
  end)
  test("cancelled prompts abort default configurations without running anything", function()
    local f = setup()
    equal(f.configuration("Launch binary").program(), f.dap.ABORT)
    equal(f.configuration("Attach to process").pid(), f.dap.ABORT)
    equal(f.configuration("Join session").session(), f.dap.ABORT)
    f.bingo.launch(); f.bingo.join(); f.bingo.attach()
    equal(#f.runs, 0)
    equal(#f.notifications, 0)
  end)
  test("plugin commands register once and reject malformed attach arguments without prompting", function()
    local f = setup()
    assert(loadfile(T.root .. "/plugin/bingo.lua"))()
    assert(loadfile(T.root .. "/plugin/bingo.lua"))()
    local count = 0
    for _ in pairs(f.commands) do count = count + 1 end
    equal(count, 5)
    f.commands.BingoAttach.callback({ args = "invalid" })
    f.commands.BingoAttach.callback({ args = "0 /x extra" })
    equal(#f.runs, 0)
    equal(#f.notifications, 2)
    f.commands.BingoLaunch.callback({ args = "/tmp/a b" })
    f.commands.BingoJoin.callback({ args = "session" })
    f.commands.BingoAttach.callback({ args = "123 /tmp/x" })
    equal(#f.runs, 3)
    f.commands.BingoSession.callback()
    T.contains(f.notifications[#f.notifications][1], "no active")
  end)
  test("real Ex commands preserve completion-escaped paths and paths with spaces", function()
    local real_api = vim.api
    local f = setup()
    local root = T.tempdir()
    local directory = root .. "/path with $HOME;[test] 'quotes' #percent%"
    assert(vim.fn.mkdir(directory) == 1)
    vim.api.nvim_create_user_command = real_api.nvim_create_user_command
    local names = { "BingoDebug", "BingoLaunch", "BingoAttach", "BingoJoin", "BingoSession" }
    T.defer(function()
      for _, name in ipairs(names) do real_api.nvim_del_user_command(name) end
    end)
    assert(loadfile(T.root .. "/plugin/bingo.lua"))()
    vim.cmd("BingoDebug " .. vim.fn.fnameescape(directory))
    vim.cmd("BingoDebug " .. root)
    vim.cmd("BingoLaunch " .. vim.fn.fnameescape(directory .. "/binary"))
    vim.cmd("BingoAttach 123 " .. vim.fn.fnameescape(directory .. "/binary"))
    vim.cmd("BingoAttach 123 " .. root .. "/raw space binary")
    equal(#f.runs, 5)
    equal(f.runs[1].program, directory)
    equal(f.runs[2].program, root)
    equal(f.runs[3].program, directory .. "/binary")
    equal(f.runs[4].binaryPath, directory .. "/binary")
    equal(f.runs[5].binaryPath, root .. "/raw space binary")
    equal(f.prompts, 0)
  end)
  local function checkhealth()
    local reports = {}
    vim.health = {}
    for _, kind in ipairs({ "start", "ok", "error", "info", "warn" }) do
      vim.health[kind] = function(message) reports[#reports + 1] = kind .. ": " .. message end
    end
    require("bingo.health").check()
    return table.concat(reports, "\n")
  end
  test("checkhealth reports prepared binary and source prerequisites without startup", function()
    local f = setup()
    vim.fn.exepath = function(name) equal(name, "go"); return "/go binary" end
    local reports = checkhealth()
    T.contains(reports, "Server binary: /prepared bingo")
    T.contains(reports, "Go for local source launches: /go binary")
    T.contains(reports, "Management 127.0.0.1:6060; DAP 127.0.0.1:4711")
    equal(#f.managers[1].requests, 0)
    equal(#f.runs, 0)
  end)
  test("checkhealth makes missing Go, native tools and server binary actionable", function()
    local f = setup()
    f.info.binary, f.info.platform = nil, "Darwin"
    f.info.binary_error, f.info.prepare_script = "no bingo server binary found", "/checkout space/scripts/prepare.sh"
    vim.fn.exepath = function() return "" end
    vim.fn.executable = function() return 0 end
    local reports = checkhealth()
    T.contains(reports, "warn: no bingo server binary found")
    T.contains(reports, "Prepare from the monorepo: bash ")
    T.contains(reports, "Go is not on PATH")
    T.contains(reports, "xcode-select --install")
    equal(#f.managers[1].requests, 0)
    equal(#f.runs, 0)
  end)
  test("connectOnly checkhealth does not require local Go, native tools or server binary", function()
    local f = setup()
    f.info = { mode = "connectOnly", dap = "remote:4711" }
    vim.fn.exepath = function() error("remote health required a local binary") end
    vim.fn.executable = function() error("remote health required local tools") end
    local reports = checkhealth()
    T.contains(reports, "connectOnly: DAP remote:4711")
    T.contains(reports, "available on the server, not this editor")
    equal(#f.managers[1].requests, 0)
    equal(#f.runs, 0)
  end)
  test("documented lazy.nvim spec loads commands after adding the monorepo runtimepath", function()
    local f = setup()
    local source = assert(T.read(T.root .. "/README.md"):match("```lua\n(.-)\n```"))
    local spec = assert(loadstring("return " .. source))()
    equal(spec[1], "bingosuite/bingo")
    equal(spec.lazy, false)
    equal(spec.dependencies[1], "mfussenegger/nvim-dap")
    local runtimepath, commands_loaded
    vim.opt = { rtp = { append = function(_, path) runtimepath = path end } }
    vim.cmd = { runtime = function(path)
      equal(path, "plugin/bingo.lua")
      assert(loadfile(T.root .. "/" .. path))()
      commands_loaded = true
    end }
    spec.config({ dir = "/installed monorepo" })
    equal(runtimepath, "/installed monorepo/editors/neovim")
    equal(commands_loaded, true)
    equal(type(f.commands.BingoDebug.callback), "function")
    equal(f.dap.configurations.go[1].name, "bingo: Debug Go package")
    equal(#f.managers[2].requests, 0)
  end)
  test("documented lazy.nvim build hook preserves argv paths and reports preparation failure", function()
    setup()
    local source = assert(T.read(T.root .. "/README.md"):match("```lua\n(.-)\n```"))
    local spec = assert(loadstring("return " .. source))()
    local command, waited
    vim.system = function(argv, options)
      command = argv
      equal(options.text, true)
      return { wait = function()
        waited = true
        return { code = 1, stdout = "compiler context", stderr = "missing tool\n" }
      end }
    end
    T.raises(function() spec.build({ dir = "/path with spaces;literal" }) end, "missing tool")
    equal(waited, true)
    equal(#command, 2)
    equal(command[1], "bash")
    equal(command[2], "/path with spaces;literal/editors/neovim/scripts/prepare.sh")
  end)
end
