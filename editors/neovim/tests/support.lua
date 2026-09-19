local T = {}
local cleanup = {}

function T.equal(actual, expected, context)
  assert(actual == expected, string.format("%s: got %s, want %s",
    context or "values differ", tostring(actual), tostring(expected)))
end

function T.contains(actual, expected)
  assert(type(actual) == "string" and actual:find(expected, 1, true),
    string.format("expected %q in %s", expected, tostring(actual)))
end

function T.raises(callback, expected)
  local ok, message = pcall(callback)
  T.equal(ok, false, "expected rejection")
  T.contains(tostring(message), expected)
end

function T.read(path)
  local file = assert(io.open(path, "rb"))
  local text = assert(file:read("*a"))
  assert(file:close())
  return text
end

function T.defer(callback)
  cleanup[#cleanup + 1] = callback
end

function T.tempdir()
  local path = vim.fn.tempname() .. " bingo test"
  assert(vim.fn.mkdir(path, "p") == 1)
  T.defer(function() assert(vim.fn.delete(path, "rf") == 0) end)
  return path
end

local function shallow(value)
  local result = {}
  for key, item in pairs(value) do
    result[key] = item
  end
  return result
end

local function restore_table(value, saved)
  for key in pairs(value) do
    value[key] = nil
  end
  for key, item in pairs(saved) do
    value[key] = item
  end
end

function T.isolate()
  local real_vim, path = _G.vim, package.path
  local loaded, preload = shallow(package.loaded), shallow(package.preload)
  local previous_cleanup = cleanup
  cleanup = {}
  return function()
    local errors = {}
    for i = #cleanup, 1, -1 do
      local ok, err = pcall(cleanup[i])
      if not ok then
        errors[#errors + 1] = tostring(err)
      end
    end
    cleanup = previous_cleanup
    _G.vim, package.path = real_vim, path
    restore_table(package.loaded, loaded)
    restore_table(package.preload, preload)
    assert(#errors == 0, table.concat(errors, "\n"))
  end
end

function T.health()
  local protocol = T.read(T.root .. "/../../pkg/protocol/protocol.go")
  local dap = T.read(T.root .. "/../../pkg/protocol/dap.go")
  local server = T.read(T.root .. "/../../internal/server/handler.go")
  return {
    service = assert(server:match('serviceIdentity%s*=%s*"([^"]+)"')),
    managementApiVersion = tonumber(assert(server:match("ManagementAPIVersion%s*=%s*(%d+)"))),
    wireProtocolVersion = assert(protocol:match('const Version%s*=%s*"([^"]+)"')),
    instanceId = "00000000-0000-4000-8000-000000000001",
    dap = {
      enabled = true,
      address = "127.0.0.1:4711",
      sessionEventVersion = tonumber(assert(dap:match("DAPSessionEventVersion%s*=%s*(%d+)"))),
      sourceLaunchVersion = tonumber(assert(dap:match("DAPSourceLaunchVersion%s*=%s*(%d+)"))),
    },
    managedIdleShutdown = { enabled = true, timeoutMs = 30000 },
    sessionCount = 0,
  }
end

function T.clock()
  local c = { now = 0, handles = {}, scheduled = {} }
  function c.schedule(callback)
    c.scheduled[#c.scheduled + 1] = callback
  end
  function c.flush()
    while #c.scheduled > 0 do
      local pending = c.scheduled
      c.scheduled = {}
      for _, callback in ipairs(pending) do
        callback()
      end
    end
  end
  function c.handle(kind)
    local h = { kind = kind, closes = 0, stops = 0, unrefs = 0 }
    function h:is_closing()
      return self.closes > 0
    end
    function h:close()
      T.equal(self.closes, 0, kind .. " closed twice")
      self.closes = self.closes + 1
    end
    function h:unref()
      self.unrefs = self.unrefs + 1
    end
    function h:start(delay, _, callback)
      self.due, self.callback = c.now + delay, callback
      return 0
    end
    function h:stop()
      self.stops = self.stops + 1
      self.due = nil
    end
    c.handles[#c.handles + 1] = h
    return h
  end
  function c.advance(ms)
    local target = c.now + ms
    for _ = 1, 10000 do
      local next_timer
      for _, h in ipairs(c.handles) do
        if not h:is_closing() and h.due and h.due <= target
          and (not next_timer or h.due < next_timer.due)
        then
          next_timer = h
        end
      end
      if not next_timer then
        c.now = target
        c.flush()
        return
      end
      c.now, next_timer.due = next_timer.due, nil
      next_timer.callback()
      c.flush()
    end
    error("timer loop did not converge")
  end
  function c.closed()
    for _, h in ipairs(c.handles) do
      T.equal(h.closes, 1, h.kind .. " leaked")
    end
  end
  c.uv = {
    new_timer = function() return c.handle("timer") end,
    hrtime = function() return c.now * 1000000 end,
  }
  return c
end

return T
