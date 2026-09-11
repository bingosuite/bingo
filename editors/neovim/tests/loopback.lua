return function(test, equal, T)
  local health = require("bingo.health")
  local uv = vim.uv
  local function serve(on_request)
    local handles, requests, failures = {}, {}, {}
    local function guard(callback)
      return function(...)
        local ok, err = pcall(callback, ...)
        if not ok then failures[#failures + 1] = tostring(err) end
      end
    end
    local function own(handle)
      assert(handle)
      handles[#handles + 1] = handle
      return handle
    end
    T.defer(function()
      for _, handle in ipairs(handles) do
        if not handle:is_closing() then handle:close() end
      end
      assert(vim.wait(1000, function()
        for _, handle in ipairs(handles) do
          if uv.is_active(handle) then return false end
        end
        return true
      end, 5), "test-owned listener/peer/timer leaked")
      equal(#failures, 0, table.concat(failures, "\n"))
    end)
    local listener = own(uv.new_tcp())
    assert(listener:bind("127.0.0.1", 0))
    assert(listener:listen(16, guard(function(err)
      assert(not err, err)
      local peer = own(uv.new_tcp())
      assert(listener:accept(peer))
      local request = ""
      assert(peer:read_start(guard(function(read_error, chunk)
        assert(not read_error, read_error)
        if not chunk then return end
        request = request .. chunk
        if request:find("\r\n\r\n", 1, true) then
          peer:read_stop()
          requests[#requests + 1] = request
          on_request(peer, own, guard)
        end
      end)))
    end)))
    return { host = "127.0.0.1", port = listener:getsockname().port }, requests
  end
  local function run_probe(endpoint, timeout)
    local results, owned = {}, {}
    local deps = {
      uv = setmetatable({
        new_tcp = function()
          local h = assert(uv.new_tcp()); owned[#owned + 1] = h; return h
        end,
        new_timer = function()
          local h = assert(uv.new_timer()); owned[#owned + 1] = h; return h
        end,
      }, { __index = uv }),
      schedule = vim.schedule, decode = vim.json.decode,
    }
    local cancel = health.probe(endpoint, { host = "127.0.0.1", port = 4711 }, timeout,
      function(result) results[#results + 1] = result end, deps)
    T.defer(function()
      cancel()
      assert(vim.wait(1000, function() return #results > 0 end, 5), "cancel did not complete")
      for _, h in ipairs(owned) do equal(h:is_closing(), true, "probe handle leaked") end
    end)
    return results, cancel
  end
  local function await(results)
    assert(vim.wait(2000, function() return #results > 0 end, 5), "health probe never completed")
    equal(#results, 1)
    return results[1]
  end

  test("real libuv keep-alive Content-Length response completes without EOF", function()
    local body = vim.json.encode(T.health())
    local endpoint, requests = serve(function(peer)
      assert(peer:write("HTTP/1.1 200 OK\r\nConnection: keep-alive\r\nContent-Length: "
        .. #body .. "\r\n\r\n" .. body))
    end)
    local result = await(run_probe(endpoint, 1000))
    equal(result.kind, "compatible")
    equal(#requests, 1)
    T.contains(requests[1], "Host: " .. endpoint.host .. ":" .. endpoint.port .. "\r\n")
  end)
  test("real libuv fragmented chunked response decodes the Go-derived contract", function()
    local body = vim.json.encode(T.health())
    local endpoint = serve(function(peer, own, guard)
      local parts = { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" }
      for i = 1, #body, 17 do
        local piece = body:sub(i, i + 16)
        parts[#parts + 1] = string.format("%x\r\n%s\r\n", #piece, piece)
      end
      parts[#parts + 1] = "0\r\n\r\n"
      local timer = own(uv.new_timer())
      local index = 0
      assert(timer:start(0, 2, guard(function()
        index = index + 1
        assert(peer:write(parts[index]))
        if index == #parts then timer:stop(); timer:close() end
      end)))
    end)
    equal(await(run_probe(endpoint, 1000)).kind, "compatible")
  end)
  test("real libuv slow-drip response hits a wall deadline despite incoming bytes", function()
    local writes = 0
    local endpoint = serve(function(peer, own, guard)
      local timer = own(uv.new_timer())
      assert(timer:start(0, 10, guard(function()
        writes = writes + 1
        peer:write("H", guard(function(err)
          if err and not timer:is_closing() then timer:stop(); timer:close() end
        end))
      end)))
    end)
    local start = uv.hrtime()
    local result = await(run_probe(endpoint, 120))
    local elapsed = (uv.hrtime() - start) / 1000000
    equal(result.kind, "transportError")
    T.contains(result.error, "timed out after 120ms")
    assert(writes >= 2, "slow-drip path was not exercised")
    assert(elapsed >= 100 and elapsed < 1500, "absolute deadline was not bounded")
  end)
  test("real libuv cancellation closes an in-progress request exactly once", function()
    local endpoint, requests = serve(function() end)
    local results, cancel = run_probe(endpoint, 1000)
    assert(vim.wait(1000, function() return #requests == 1 end, 5), "request never reached peer")
    cancel(); cancel()
    T.contains(await(results).error, "cancelled")
  end)
  test("real libuv EOF rejects a truncated body instead of decoding a partial success", function()
    local endpoint = serve(function(peer, _, guard)
      assert(peer:write("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n{}", guard(function(err)
        assert(not err, err)
        assert(peer:shutdown(guard(function(shutdown_error)
          assert(not shutdown_error, shutdown_error)
          peer:close()
        end)))
      end)))
    end)
    T.contains(await(run_probe(endpoint, 1000)).error, "incomplete HTTP body")
  end)
  test("real libuv concurrent health probes remain independent and release all client handles", function()
    local body = vim.json.encode(T.health())
    local endpoint, requests = serve(function(peer)
      assert(peer:write("HTTP/1.1 200 OK\r\nContent-Length: " .. #body .. "\r\n\r\n" .. body))
    end)
    local probes = {}
    for i = 1, 30 do probes[i] = run_probe(endpoint, 1000) end
    for _, results in ipairs(probes) do equal(await(results).kind, "compatible") end
    equal(#requests, 30)
  end)
end
