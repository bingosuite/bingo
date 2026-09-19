return function(test, equal, T)
  local config, server = require("bingo.config"), require("bingo.server")
  local compatible = { kind = "compatible", health = { instance_id = "ready" } }
  local function setup(options, change)
    local c = T.clock()
    c.probes, c.spawns, c.logs, c.notifications, c.opens, c.closes = {}, {}, {}, {}, {}, {}
    c.uv.os_uname = function() return { sysname = "Darwin", machine = "arm64" } end
    c.uv.fs_open = function(path, mode, permissions)
      c.opens[#c.opens + 1] = { path, mode, permissions }
      return 17
    end
    c.uv.fs_close = function(fd)
      c.closes[#c.closes + 1] = fd
      return true
    end
    c.uv.spawn = function(binary, args, on_exit)
      local handle = c.handle("process")
      local pid = 4242 + #c.spawns
      c.spawns[#c.spawns + 1] = { binary = binary, options = args, exit = on_exit, handle = handle, pid = pid }
      return handle, pid
    end
    c.uv.kill = function() error("must never signal a shared server") end
    local deps = {
      uv = c.uv, schedule = c.schedule,
      executable = function() return 1 end,
      exepath = function(path) return path == "bingo" and "/usr/bin/bingo" or path end,
      mkdir = function() end,
      stdpath = function() return "/isolated state" end,
      notify = function(message) c.notifications[#c.notifications + 1] = message end,
      probe = function(endpoint, dap, timeout, callback)
        local p = { endpoint = endpoint, dap = dap, timeout = timeout, callback = callback, cancels = 0 }
        c.probes[#c.probes + 1] = p
        return function()
          p.cancels = p.cancels + 1
          callback({ kind = "transportError", error = "cancelled" })
        end
      end,
    }
    options = options or {}
    options.log = options.log or function(message) c.logs[#c.logs + 1] = message end
    if change then change(c, deps) end
    local manager = server.new(config.normalize(options), deps)
    T.defer(function() manager:dispose(); c.flush(); c.closed() end)
    c.manager, c.deps = manager, deps
    c.results = {}
    function c.ensure(overrides, callback)
      manager:ensure(overrides or {}, callback or function(err, endpoint)
        c.results[#c.results + 1] = { error = err, endpoint = endpoint }
      end)
    end
    function c.reply(index, result)
      c.probes[index].callback(result)
      c.flush()
    end
    return c
  end

  test("hundreds of same-endpoint ensures coalesce and each completes exactly once", function()
    local c = setup()
    for _ = 1, 500 do c.ensure() end
    equal(#c.probes, 1)
    c.reply(1, compatible)
    equal(#c.results, 500)
    equal(#c.spawns, 0)
    equal(next(c.manager.in_flight), nil)
    for _, result in ipairs(c.results) do equal(result.error, nil); equal(result.endpoint.port, 4711) end
    c.reply(1, { kind = "absent" })
    equal(#c.results, 500)
    equal(#c.spawns, 0)
  end)
  test("independent endpoint attempts can complete out of order", function()
    local c = setup()
    c.ensure()
    c.ensure({ managementPort = 6061, dapPort = 4712 })
    equal(#c.probes, 2)
    c.reply(2, compatible)
    equal(c.results[1].endpoint.port, 4712)
    equal(#c.results, 1)
    c.reply(1, compatible)
    equal(c.results[2].endpoint.port, 4711)
  end)
  test("normalized endpoints share one coalesced attempt", function()
    local c = setup()
    c.ensure()
    c.ensure({ managementHost = " 127.0.0.1 ", dapHost = "127.0.0.1 " })
    equal(#c.probes, 1)
    c.reply(1, compatible)
    equal(#c.results, 2)
  end)
  test("callback exceptions cannot starve later waiters or a reentrant ensure", function()
    local c = setup()
    c.ensure({}, function()
      c.ensure()
      error("injected waiter failure")
    end)
    c.ensure()
    c.reply(1, compatible)
    equal(#c.results, 1)
    equal(#c.probes, 2)
    T.contains(c.notifications[1], "injected waiter failure")
    c.reply(2, compatible)
    equal(#c.results, 2)
  end)
  test("repeated independent ensure/dispose cycles retain no attempt or process handle", function()
    for _ = 1, 100 do
      local c = setup()
      c.ensure()
      c.reply(1, { kind = "absent" })
      c.reply(2, compatible)
      equal(#c.spawns, 1)
      c.manager:dispose()
      equal(next(c.manager.in_flight), nil)
      equal(next(c.manager.processes), nil)
      equal(next(c.manager.timers), nil)
      c.closed()
    end
  end)
  for _, operation in ipairs({ "allocate", "start" }) do
    for _, shape in ipairs({ "return", "throw" }) do
      test("readiness timer " .. operation .. " " .. shape .. " failure settles and closes ownership", function()
        local c = setup(nil, function(clock)
          local new_timer = clock.uv.new_timer
          clock.uv.new_timer = function()
            local function failure()
              if shape == "throw" then error("injected timer failure") end
              return nil, "injected timer failure"
            end
            if operation == "allocate" then return failure() end
            local timer = new_timer()
            timer.start = failure
            return timer
          end
        end)
        c.ensure()
        c.reply(1, { kind = "absent" })
        c.reply(2, { kind = "absent" })
        equal(#c.results, 1)
        T.contains(c.results[1].error, "injected timer failure")
        equal(next(c.manager.in_flight), nil)
        equal(next(c.manager.timers), nil)
      end)
    end
  end
  for _, host in ipairs({ "localhost", "::1", "0.0.0.0", "debug.internal", "127.0.0.2" }) do
    test("auto refuses non-default host before probing: " .. host, function()
      local c = setup()
      c.ensure({ managementHost = host })
      T.contains(c.results[1].error, "requires managementHost and dapHost")
      equal(#c.probes, 0)
      equal(#c.spawns, 0)
    end)
  end
  for _, platform in ipairs({
    { "Linux", "x86_64", true }, { "Linux", "amd64", true },
    { "Darwin", "arm64", true }, { "Darwin", "aarch64", true },
    { "Darwin", "x86_64", false }, { "Linux", "aarch64", false }, { "Windows_NT", "AMD64", false },
  }) do
    test("native startup platform boundary: " .. platform[1] .. "/" .. platform[2], function()
      local c = setup(nil, function(clock)
        clock.uv.os_uname = function() return { sysname = platform[1], machine = platform[2] } end
      end)
      c.ensure()
      equal(#c.probes, platform[3] and 1 or 0)
      if platform[3] then c.reply(1, compatible)
      else T.contains(c.results[1].error, "supports only") end
    end)
  end
  test("connectOnly bypasses platform/probe/spawn but cancellation wins queued completion", function()
    local c = setup(nil, function(clock)
      clock.uv.os_uname = function() error("connectOnly inspected platform") end
    end)
    c.ensure({ serverMode = "connectOnly", dapHost = "debug.internal" })
    equal(#c.results, 0)
    c.flush()
    equal(c.results[1].endpoint.host, "debug.internal")
    equal(#c.probes, 0)
    c.ensure({ serverMode = "connectOnly" })
    c.manager:dispose()
    c.flush()
    equal(#c.results, 2)
    T.contains(c.results[2].error, "cancelled")
    c.ensure()
    c.flush()
    T.contains(c.results[3].error, "disposed")
  end)
  test("auto endpoint collision fails before every side effect", function()
    local c = setup()
    c.ensure({ dapPort = 6060 })
    c.flush()
    T.contains(c.results[1].error, "distinct")
    equal(#c.probes, 0)
    equal(#c.opens, 0)
  end)
  for _, result in ipairs({
    { kind = "incompatible", reason = "wrong service" },
    { kind = "transportError", error = "timeout" },
    { kind = "unexpected" },
  }) do
    test("only proven connection refusal authorizes spawn: " .. result.kind, function()
      local c = setup()
      c.ensure()
      c.reply(1, result)
      equal(#c.results, 1)
      assert(c.results[1].error)
      equal(#c.spawns, 0)
      equal(#c.opens, 0)
    end)
  end
  test("argv and persistent log descriptors are literal and closed once", function()
    local binary = "/tmp/a b/$(not-shell);bingo"
    local log = "/tmp/log directory/server log"
    local c = setup({ server = { binary = binary, log_path = log, idle_timeout_ms = 321 } })
    c.ensure({ managementPort = 6061, dapPort = 4712 })
    c.reply(1, { kind = "absent" })
    local spawn = c.spawns[1]
    equal(spawn.binary, binary)
    equal(vim.inspect(spawn.options.args),
      vim.inspect({ "-addr", "127.0.0.1:6061", "-dap-addr", "127.0.0.1:4712", "-idle-timeout", "321ms" }))
    equal(spawn.options.detached, true)
    equal(spawn.options.stdio[1], nil)
    equal(spawn.options.stdio[2], 17)
    equal(spawn.options.stdio[3], 17)
    equal(c.opens[1][1], log)
    equal(c.opens[1][2], "a")
    equal(c.opens[1][3], 420)
    equal(#c.closes, 1)
    equal(c.closes[1], 17)
    equal(spawn.handle.unrefs, 1)
    c.reply(2, compatible)
  end)
  for _, source in ipairs({ "explicit-path", "explicit-command", "bundled", "PATH", "missing" }) do
    test("binary resolution precedence: " .. source, function()
      local explicit = source:match("^explicit") and "/configured bingo" or nil
      local c = setup({ server = { binary = explicit } }, function(_, deps)
        deps.exepath = function(name)
          if source == "explicit-command" and name == explicit then return "/resolved bingo" end
          if name == "bingo" and source == "PATH" then return "/path bingo" end
          return ""
        end
        deps.executable = function(name)
          if source == "explicit-path" and name == explicit then return 1 end
          if source == "bundled" and name:match("/bin/bingo$") then return 1 end
          return 0
        end
      end)
      c.ensure(); c.reply(1, { kind = "absent" })
      if source == "missing" then
        equal(#c.spawns, 0)
        T.contains(c.results[1].error, "no bingo server binary found")
      else
        equal(#c.spawns, 1)
        if source == "bundled" then
          T.contains(c.spawns[1].binary, "editors/neovim/bin/bingo")
        else
          equal(c.spawns[1].binary, source == "explicit-path" and explicit
            or source == "explicit-command" and "/resolved bingo" or "/path bingo")
        end
        c.reply(2, compatible)
      end
    end)
  end
  test("diagnostic inspection shares binary resolution without probing or starting anything", function()
    local c = setup()
    local info = server.inspect(config.normalize({ server = { binary = "/configured bingo" } }), c.deps)
    equal(info.binary, "/configured bingo")
    equal(info.platform_supported, true)
    equal(info.management, "127.0.0.1:6060")
    equal(info.dap, "127.0.0.1:4711")
    T.contains(info.prepare_script, "editors/neovim/scripts/prepare.sh")
    equal(#c.probes, 0)
    equal(#c.opens, 0)
    equal(#c.spawns, 0)
  end)
  test("connectOnly diagnostics never inspect local platform or binaries", function()
    local c = setup(nil, function(clock, deps)
      clock.uv.os_uname = function() error("remote native platform check") end
      deps.executable = function() error("remote executable check") end
      deps.exepath = function() error("remote PATH check") end
    end)
    local info = server.inspect(config.normalize({
      server = { mode = "connectOnly", dap_host = "debug.internal" },
    }), c.deps)
    equal(info.mode, "connectOnly")
    equal(info.dap, "debug.internal:4711")
    equal(info.binary, nil)
    equal(#c.probes, 0)
    equal(#c.spawns, 0)
  end)
  test("bundled server path stays absolute when the editor changes cwd after loading bingo", function()
    local c = setup()
    local before = server.inspect(config.normalize(), c.deps).binary
    equal(before:sub(1, 1), "/")
    local cwd, temporary = vim.fn.getcwd(), T.tempdir()
    T.defer(function() vim.fn.chdir(cwd) end)
    vim.fn.chdir(temporary)
    equal(server.inspect(config.normalize(), c.deps).binary, before)
  end)
  test("a child losing listener arbitration is success when a compatible winner appears", function()
    local c = setup()
    c.ensure()
    c.reply(1, { kind = "absent" })
    c.spawns[1].exit(1, 0)
    c.flush()
    equal(next(c.manager.processes), nil)
    c.reply(2, compatible)
    equal(c.results[1].error, nil)
    equal(c.spawns[1].handle.closes, 1)
  end)
  for _, kind in ipairs({ "return", "throw" }) do
    test("spawn failure (" .. kind .. ") closes fd and releases all waiters", function()
      local c = setup(nil, function(clock)
        clock.uv.spawn = function()
          if kind == "throw" then error("EACCES") end
          return nil, "EACCES"
        end
      end)
      c.ensure(); c.ensure()
      c.reply(1, { kind = "absent" })
      equal(#c.results, 2)
      T.contains(c.results[1].error, "EACCES")
      equal(#c.closes, 1)
      equal(next(c.manager.in_flight), nil)
    end)
  end
  for _, stage in ipairs({ "binary", "mkdir", "open", "close" }) do
    test("startup prerequisite failure names " .. stage .. " and does not strand waiters", function()
      local c = setup({ server = { binary = "/missing" } }, function(clock, deps)
        if stage == "binary" then
          deps.exepath = function() return "" end
          deps.executable = function() return 0 end
        elseif stage == "mkdir" then deps.mkdir = function() error("mkdir failed") end
        elseif stage == "open" then clock.uv.fs_open = function() return nil, "open failed" end
        else clock.uv.fs_close = function() return nil, "close failed" end end
      end)
      c.ensure()
      c.reply(1, { kind = "absent" })
      equal(#c.results, 1)
      T.contains(c.results[1].error, stage == "binary" and "not executable" or stage == "mkdir" and "directory" or stage)
      equal(next(c.manager.in_flight), nil)
      equal(#c.spawns, stage == "close" and 1 or 0)
    end)
  end
  for _, stage in ipairs({ "binary", "mkdir", "open" }) do
    test("disposal during " .. stage .. " prerequisite cannot spawn a child", function()
      local c = setup()
      if stage == "binary" then
        c.deps.executable = function() return 0 end
        c.deps.exepath = function(path) c.manager:dispose(); return path end
      elseif stage == "mkdir" then c.deps.mkdir = function() c.manager:dispose() end
      else c.uv.fs_open = function() c.manager:dispose(); return 17 end end
      c.ensure()
      c.reply(1, { kind = "absent" })
      equal(#c.results, 1)
      T.contains(c.results[1].error, "cancelled")
      equal(#c.spawns, 0)
      if stage == "open" then equal(#c.closes, 1) end
    end)
  end
  test("readiness spends only remaining absolute budget, including delayed callbacks", function()
    local c = setup({ server = { ready_timeout_ms = 250 } })
    c.ensure()
    c.reply(1, { kind = "absent" })
    equal(c.probes[2].timeout, 250)
    c.now = 180
    c.reply(2, { kind = "transportError", error = "drip" })
    c.advance(70)
    equal(#c.results, 1)
    equal(#c.probes, 2)
    T.contains(c.results[1].error, "within 250ms")
    T.contains(c.results[1].error, "drip")
    equal(c.now, 250)
  end)
  test("second readiness probe gets the reduced budget", function()
    local c = setup({ server = { ready_timeout_ms = 500 } })
    c.ensure(); c.reply(1, { kind = "absent" })
    c.now = 120; c.reply(2, { kind = "absent" }); c.advance(100)
    equal(c.probes[3].timeout, 280)
    c.reply(3, compatible)
    equal(c.results[1].error, nil)
  end)
  test("late compatible readiness cannot succeed after the absolute deadline", function()
    local c = setup({ server = { ready_timeout_ms = 100 } })
    c.ensure(); c.reply(1, { kind = "absent" })
    c.now = 101; c.reply(2, compatible)
    T.contains(c.results[1].error, "within 100ms")
  end)
  test("dispose cancels an active health probe and fences all queued retry work", function()
    local c = setup()
    c.ensure(); c.reply(1, { kind = "absent" })
    c.reply(2, { kind = "absent" })
    local timer = c.handles[#c.handles]
    c.manager:dispose()
    timer.callback()
    c.advance(10000)
    equal(#c.probes, 2)
    equal(#c.results, 1)
    equal(next(c.manager.timers), nil)
    c.closed()
  end)
  test("disposal cancels pending probes once without killing children", function()
    local c = setup()
    c.ensure(); c.reply(1, { kind = "absent" })
    c.manager:dispose(); c.manager:dispose()
    equal(c.probes[2].cancels, 1)
    c.reply(2, compatible)
    equal(#c.results, 1)
    equal(c.spawns[1].handle.closes, 1)
  end)
  test("duplicate absence callback cannot spawn another child or overwrite a readiness probe", function()
    local c = setup()
    c.ensure(); c.reply(1, { kind = "absent" }); c.reply(1, { kind = "absent" })
    equal(#c.spawns, 1)
    equal(#c.probes, 2)
    c.reply(2, compatible)
    equal(#c.results, 1)
  end)
end
