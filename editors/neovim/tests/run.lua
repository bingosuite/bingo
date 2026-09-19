local root = assert(arg[0]:match("^(.*)/tests/run%.lua$"))
package.path = root .. "/lua/?.lua;" .. root .. "/lua/?/init.lua;"
  .. root .. "/tests/?.lua;" .. package.path

for _, path in ipairs(vim.fn.glob(root .. "/**/*.lua", false, true)) do
  assert(loadfile(path))
end

local T = require("support")
T.root = root
local failures, tests = 0, 0
local names = {}
local function test(name, callback)
  assert(not names[name], "duplicate test name: " .. name)
  names[name] = true
  tests = tests + 1
  local restore = T.isolate()
  local ok, failure = xpcall(callback, debug.traceback)
  local cleaned, cleanup_error = pcall(restore)
  if not cleaned then
    ok, failure = false, tostring(failure or "") .. "\n" .. tostring(cleanup_error)
  end
  if ok then
    io.write("ok - " .. name .. "\n")
  else
    failures = failures + 1
    io.stderr:write("not ok - " .. name .. ": " .. tostring(failure) .. "\n")
  end
end

for _, suite in ipairs({ "legacy", "contracts", "startup", "prepare", "transport", "manager", "plugin", "loopback" }) do
  require(suite)(test, T.equal, T)
end

if failures > 0 then
  io.stderr:write(string.format("%d of %d tests failed\n", failures, tests))
  os.exit(1)
end
io.write(string.format("%d tests passed\n", tests))
