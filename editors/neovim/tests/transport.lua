return function(test, equal, T)
  local health = require("bingo.health")
  local http = require("bingo.http")
  local endpoint = { host = "127.0.0.1", port = 6060 }
  local dap = { host = "127.0.0.1", port = 4711 }
  local function response(body)
    return "HTTP/1.1 200 OK\r\nContent-Length: " .. #body .. "\r\n\r\n" .. body
  end
  local function probe(change)
    local c = T.clock()
    local tcp = c.handle("tcp")
    c.uv.new_tcp = function() return tcp end
    tcp.writes, tcp.reads = 0, 0
    function tcp:connect(host, port, callback)
      self.host, self.port, self.connected = host, port, callback
      return {}
    end
    function tcp:write(request, callback)
      self.request, self.written = request, callback
      self.writes = self.writes + 1
      return {}
    end
    function tcp:read_start(callback)
      self.read = callback
      self.reads = self.reads + 1
      return 0
    end
    local deps = { uv = c.uv, schedule = c.schedule, decode = vim.json.decode }
    local results = {}
    if change then change(c, tcp, deps) end
    local cancel = health.probe(endpoint, dap, 100, function(result)
      results[#results + 1] = result
    end, deps)
    T.defer(cancel)
    return c, tcp, results, cancel
  end
  local function reading(tcp)
    tcp.connected(nil)
    tcp.written(nil)
  end

  test("framed multibyte body completes at its byte length without EOF", function()
    local body = vim.json.encode({ message = "\195\169\240\159\152\128" })
    local wire = response(body)
    local parsed = assert(http.parse(wire, false))
    equal(parsed.body, body)
    for i = 1, #wire - 1 do
      local partial, err = http.parse(wire:sub(1, i), false)
      equal(partial, nil, "prefix " .. i)
      equal(err, nil, "prefix " .. i)
    end
  end)
  test("close-delimited HTTP/1.0 response requires EOF", function()
    local raw = "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n{}"
    equal(http.parse(raw, false), nil)
    equal(assert(http.parse(raw, true)).body, "{}")
  end)
  test("chunked framing tolerates every split including extensions and trailers", function()
    local raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
      .. "1;name=value\r\n{\r\n1\r\n}\r\n0\r\nX-Trace: done\r\n\r\n"
    for i = 1, #raw - 1 do
      local value, err = http.parse(raw:sub(1, i), false)
      equal(value, nil, "chunk prefix " .. i)
      equal(err, nil, "chunk prefix " .. i)
    end
    equal(assert(http.parse(raw, false)).body, "{}")
    equal(assert(http.parse("HTTP/1.1 200 OK\r\nTransfer-Encoding: CHUNKED\r\n\r\n0\r\n\r\n", false)).body, "")
  end)
  for _, entry in ipairs({
    { "HTTP/2.0 200 OK\r\n\r\n{}", "status line" },
    { "HTTP/1.1 2000 bad\r\n\r\n{}", "status line" },
    { "HTTP/1.1 200 OK\r\nMissingColon\r\n\r\n{}", "invalid HTTP header" },
    { "HTTP/1.1 200 OK\r\n Fold: x\r\n\r\n{}", "invalid HTTP header" },
    { "HTTP/1.1 200 OK\r\nX: a\nb\r\n\r\n{}", "invalid HTTP header" },
    { "HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n", "Content-Length" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 1.5\r\n\r\n", "Content-Length" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 2, 2\r\n\r\n{}", "Content-Length" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 65537\r\n\r\n", "exceeded" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 2\r\ncontent-length: 2\r\n\r\n{}", "duplicate" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n{}", "conflicting" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip\r\n\r\n{}", "unsupported" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked, gzip\r\n\r\n{}", "unsupported" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nq\r\n", "chunk size" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n10001\r\n", "exceeded" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nxXX", "terminator" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nContent-Length: 0\r\n\r\n", "framing headers" },
    { response("{}") .. "extra", "trailing bytes" },
    { "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nx", "trailing bytes" },
    { "HTTP/1.1 200 OK\r\nX: " .. string.rep("x", 8192), "headers exceeded" },
    { string.rep("x", 65537), "64 KiB" },
  }) do
    test("HTTP rejects malformed framing: " .. entry[2] .. " (" .. #entry[1] .. " bytes)", function()
      local value, err = http.parse(entry[1], true)
      equal(value, nil)
      T.contains(err, entry[2])
    end)
  end
  for _, raw in ipairs({
    "HTTP/1.1 200", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{",
    "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1",
    "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx",
    "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX: y",
  }) do
    test("EOF rejects truncated framing (" .. #raw .. " bytes)", function()
      local value, err = http.parse(raw, true)
      equal(value, nil)
      T.contains(err, "incomplete")
    end)
  end
  test("wire limit accepts exactly 64 KiB and rejects one byte more", function()
    local head = "HTTP/1.0 200 OK\r\n\r\n"
    local raw = head .. string.rep("x", http.maximum_response_bytes - #head)
    equal(#assert(http.parse(raw, true)).body, http.maximum_response_bytes - #head)
    local value, err = http.parse(raw .. "x", true)
    equal(value, nil)
    T.contains(err, "64 KiB")
  end)
  test("chunked trailers enforce their own cap before the final delimiter", function()
    local head = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX: "
    equal(assert(http.parse(head .. string.rep("x", 8192 - 7) .. "\r\n\r\n", false)).body, "")
    local raw = head
      .. string.rep("x", 8192)
    for _, ending in ipairs({ "", "\r\n\r\n" }) do
      local value, err = http.parse(raw .. ending, false)
      equal(value, nil)
      T.contains(err, "trailers exceeded")
    end
  end)

  test("probe fragments a Go-derived health response and closes before delivering once", function()
    local c, tcp, results, cancel = probe()
    reading(tcp)
    T.contains(tcp.request, "GET /api/health HTTP/1.1\r\nHost: 127.0.0.1:6060\r\n")
    T.contains(tcp.request, "Connection: close\r\n")
    local raw = response(vim.json.encode(T.health()))
    for i = 1, #raw do tcp.read(nil, raw:sub(i, i)) end
    c.closed()
    equal(#results, 0, "completion must be scheduled")
    tcp.read(nil, nil)
    cancel()
    c.advance(1000)
    equal(#results, 1)
    equal(results[1].kind, "compatible")
  end)
  for _, entry in ipairs({
    { "connect", "ECONNREFUSED", "absent" },
    { "connect", "EHOSTUNREACH", "transportError" },
    { "write", "EPIPE", "transportError" },
    { "read", "ECONNRESET", "transportError" },
  }) do
    test("probe classifies " .. entry[1] .. " " .. entry[2] .. " and closes resources", function()
      local c, tcp, results = probe()
      if entry[1] == "connect" then tcp.connected(entry[2])
      else
        tcp.connected(nil)
        if entry[1] == "write" then tcp.written(entry[2])
        else tcp.written(nil); tcp.read(entry[2]) end
      end
      c.flush()
      equal(#results, 1)
      equal(results[1].kind, entry[3])
      c.closed()
    end)
  end
  for _, phase in ipairs({ "connect", "write", "read" }) do
    for _, action in ipairs({ "timeout", "cancel" }) do
      test(action .. " fences stale " .. phase .. " callbacks", function()
        local c, tcp, results, cancel = probe()
        if phase ~= "connect" then tcp.connected(nil) end
        if phase == "read" then tcp.written(nil) end
        local writes, reads = tcp.writes, tcp.reads
        if action == "timeout" then c.advance(100) else cancel(); c.flush() end
        if phase == "connect" then tcp.connected(nil)
        elseif phase == "write" then tcp.written(nil)
        else tcp.read(nil, response(vim.json.encode(T.health()))) end
        c.flush()
        equal(#results, 1)
        equal(results[1].kind, "transportError")
        T.contains(results[1].error, action == "cancel" and "cancelled" or "timed out")
        equal(tcp.writes, writes)
        equal(tcp.reads, reads)
        c.closed()
      end)
    end
  end
  for _, method in ipairs({ "new_tcp", "new_timer", "start", "connect", "write", "read_start" }) do
    for _, throws in ipairs({ false, true }) do
      test("synchronous libuv " .. method .. (throws and " throws" or " fails") .. " without leaks", function()
        local c, tcp, results = probe(function(clock, socket)
          local failure = function()
            if throws then error("injected " .. method) end
            return nil, "injected " .. method
          end
          if method == "new_tcp" then
            clock.uv.new_tcp = failure
            table.remove(clock.handles, 1)
          elseif method == "new_timer" then clock.uv.new_timer = failure
          elseif method == "start" then
            clock.uv.new_timer = function()
              local timer = clock.handle("timer")
              timer.start = failure
              return timer
            end
          else socket[method] = failure end
        end)
        if method == "write" or method == "read_start" then tcp.connected(nil) end
        if method == "read_start" then tcp.written(nil) end
        c.flush()
        equal(#results, 1)
        equal(results[1].kind, "transportError")
        T.contains(results[1].error, "injected " .. method)
        c.closed()
      end)
    end
  end
  test("slow drip cannot reset the absolute health deadline", function()
    local c, tcp, results = probe()
    reading(tcp)
    for _ = 1, 9 do c.advance(10); tcp.read(nil, "H") end
    equal(#results, 0)
    c.advance(10)
    equal(#results, 1)
    T.contains(results[1].error, "timed out after 100ms")
    c.closed()
  end)
  for _, entry in ipairs({
    { response("not JSON"), "incompatible", "JSON" },
    { "HTTP/1.1 503 Unavailable\r\nContent-Length: 3\r\n\r\nbad", "incompatible", "HTTP 503" },
    { "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{", "transportError", "incomplete" },
    { response("null"), "incompatible", "object" },
    { response("{}"), "incompatible", "identity" },
    { string.rep("x", 65537), "transportError", "64 KiB" },
  }) do
    test("probe surfaces malformed response: " .. entry[3], function()
      local c, tcp, results = probe()
      reading(tcp)
      tcp.read(nil, entry[1])
      tcp.read(nil, nil)
      c.flush()
      equal(#results, 1)
      equal(results[1].kind, entry[2])
      T.contains(results[1].error or results[1].reason, entry[3])
      c.closed()
    end)
  end
end
