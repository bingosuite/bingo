local M = {}

M.defaults = {
  configurations = true,
  notify_session = true,
  log = nil,
  server = {
    mode = "auto",
    management_host = "127.0.0.1",
    management_port = 6060,
    dap_host = "127.0.0.1",
    dap_port = 4711,
    ready_timeout_ms = 5000,
    idle_timeout_ms = 30000,
    binary = nil,
    log_path = nil,
  },
}

local function fail(message)
  error("bingo: " .. message, 0)
end

local function copy(value)
  if type(value) ~= "table" then
    return value
  end
  local result = {}
  for key, item in pairs(value) do
    result[key] = copy(item)
  end
  return result
end

local function boolean(value, fallback, name)
  if value == nil then
    return fallback
  end
  if type(value) ~= "boolean" then
    fail(name .. " must be a boolean")
  end
  return value
end

local function string_value(value, fallback, name)
  if value == nil then
    return fallback
  end
  if type(value) ~= "string" or value:match("^%s*$") or value:find("%z") then
    fail(name .. " must be a non-empty string")
  end
  return value
end

local function ipv6(value)
  local address = value:match("^(.-)%%[%w_.%-]+$") or value
  local tail = address:match("([^:]+)$")
  local count = 0
  if tail and tail:find(".", 1, true) then
    local octets = { tail:match("^(%d+)%.(%d+)%.(%d+)%.(%d+)$") }
    if #octets ~= 4 then
      return false
    end
    for _, octet in ipairs(octets) do
      if tonumber(octet) > 255 or #octet > 3 then
        return false
      end
    end
    address = address:sub(1, #address - #tail) .. "0:0"
  end
  for segment in address:gmatch("[^:]+") do
    if #segment > 4 or segment:find("[^%da-f]") then
      return false
    end
    count = count + 1
  end
  local compression = address:find("::", 1, true)
  if compression then
    return count < 8 and not address:find("::", compression + 2, true)
      and not address:find(":::", 1, true)
  end
  return count == 8 and address:sub(1, 1) ~= ":" and address:sub(-1) ~= ":"
end

local function host_value(value, fallback, name)
  value = string_value(value, fallback, name):match("^%s*(.-)%s*$")
  local address, zone = value:match("^(.-)(%%[^%%]+)$")
  value = address and address:lower() .. zone or value:lower()
  if value:sub(1, 1) == "[" and value:sub(-1) == "]" then
    value = value:sub(2, -2)
    if not ipv6(value) then
      fail(name .. " must be a valid bracketed IPv6 address")
    end
  end
  -- Hosts are interpolated into HTTP headers, not just passed to libuv.
  if value == "" or value:find("[^A-Za-z0-9._:%%%-]") then
    fail(name .. " must be a host without a scheme, path, or control characters")
  end
  if value:find(":", 1, true) and not ipv6(value) then
    fail(name .. " must be a valid IPv6 address")
  end
  if not value:find(":", 1, true) and value:find("%%") then
    fail(name .. " has an invalid host delimiter")
  end
  return value
end

local function integer(value, fallback, name, minimum, maximum)
  if value == nil then
    return fallback
  end
  if type(value) ~= "number" or value % 1 ~= 0 or value < minimum or value > maximum then
    fail(
      string.format(
        "%s must be an integer between %d and %d",
        name,
        minimum,
        maximum
      )
    )
  end
  return value
end

local function mode(value, fallback)
  value = value == nil and fallback or value
  if value ~= "auto" and value ~= "connectOnly" then
    fail('server mode must be "auto" or "connectOnly"')
  end
  return value
end

function M.normalize(options)
  options = options == nil and {} or options
  if type(options) ~= "table" then
    fail("setup options must be a table")
  end
  local server = options.server == nil and {} or options.server
  if type(server) ~= "table" then
    fail("server options must be a table")
  end

  local defaults = M.defaults
  local result = copy(defaults)
  result.configurations = boolean(
    options.configurations,
    defaults.configurations,
    "configurations"
  )
  result.notify_session = boolean(
    options.notify_session,
    defaults.notify_session,
    "notify_session"
  )
  if options.log ~= nil and type(options.log) ~= "function" then
    fail("log must be a function")
  end
  result.log = options.log

  result.server.mode = mode(server.mode, defaults.server.mode)
  result.server.management_host = host_value(
    server.management_host,
    defaults.server.management_host,
    "server.management_host"
  )
  result.server.management_port = integer(
    server.management_port,
    defaults.server.management_port,
    "server.management_port",
    1,
    65535
  )
  result.server.dap_host = host_value(
    server.dap_host,
    defaults.server.dap_host,
    "server.dap_host"
  )
  result.server.dap_port = integer(
    server.dap_port,
    defaults.server.dap_port,
    "server.dap_port",
    1,
    65535
  )
  result.server.ready_timeout_ms = integer(
    server.ready_timeout_ms,
    defaults.server.ready_timeout_ms,
    "server.ready_timeout_ms",
    100,
    120000
  )
  result.server.idle_timeout_ms = integer(
    server.idle_timeout_ms,
    defaults.server.idle_timeout_ms,
    "server.idle_timeout_ms",
    1,
    86400000
  )
  result.server.binary = string_value(
    server.binary,
    defaults.server.binary,
    "server.binary"
  )
  result.server.log_path = string_value(
    server.log_path,
    defaults.server.log_path,
    "server.log_path"
  )
  return result
end

local function debug_integer(debug_config, key, fallback, minimum, maximum)
  return integer(debug_config[key], fallback, key, minimum, maximum)
end

function M.resolve(debug_config, options)
  if type(debug_config) ~= "table" then
    fail("debug configuration must be a table")
  end
  options = options or M.normalize()
  local server = options.server
  local resolved_mode = mode(debug_config.serverMode, server.mode)

  local resolved = {
    mode = resolved_mode,
    management = {
      host = host_value(debug_config.managementHost, server.management_host, "managementHost"),
      port = debug_integer(
        debug_config,
        "managementPort",
        server.management_port,
        1,
        65535
      ),
    },
    dap = {
      host = host_value(debug_config.dapHost, server.dap_host, "dapHost"),
      port = debug_integer(debug_config, "dapPort", server.dap_port, 1, 65535),
    },
    ready_timeout_ms = debug_integer(
      debug_config,
      "serverReadyTimeoutMs",
      server.ready_timeout_ms,
      100,
      120000
    ),
    idle_timeout_ms = debug_integer(
      debug_config,
      "managedIdleTimeoutMs",
      server.idle_timeout_ms,
      1,
      86400000
    ),
    binary = server.binary,
    log_path = server.log_path,
  }
  if resolved.mode == "auto"
    and resolved.management.host == resolved.dap.host
    and resolved.management.port == resolved.dap.port
  then
    fail("management and DAP endpoints must be distinct in auto mode")
  end
  return resolved
end

function M.endpoint(endpoint)
  local host = endpoint.host
  if host:find(":", 1, true) then
    host = "[" .. host .. "]"
  end
  return string.format("%s:%d", host, endpoint.port)
end

function M.validate_request(value)
  if type(value) ~= "table" then
    fail("debug configuration must be a table")
  end
  boolean(value.stopOnEntry, nil, "stopOnEntry")
  if value.request == "launch" then
    string_value(value.program, nil, "program")
    if value.program == nil then
      fail("program is required")
    end
    if value.pid ~= nil or value.session ~= nil or value.binaryPath ~= nil then
      fail("launch cannot specify pid, session, or binaryPath")
    end
    for _, key in ipairs({ "args", "env" }) do
      local items = value[key]
      if items ~= nil then
        if type(items) ~= "table" or not vim.islist(items) then
          fail(key .. " must be an array of strings")
        end
        for _, item in ipairs(items) do
          if type(item) ~= "string" or item:find("%z") then
            fail(key .. " must be an array of strings without NUL bytes")
          end
        end
      end
    end
  elseif value.request == "attach" then
    if value.program ~= nil then
      fail("attach cannot specify program")
    end
    if (value.session ~= nil) == (value.pid ~= nil) then
      fail("attach requires exactly one of session or pid")
    end
    if value.session ~= nil then
      local session = require("bingo.session")
      local announcement, message = session.decode({
        version = session.event_version,
        sessionId = value.session,
      })
      if announcement == nil then
        fail(message)
      end
    else
      integer(value.pid, nil, "pid", 1, 2147483647)
    end
    if value.binaryPath ~= nil
      and (type(value.binaryPath) ~= "string" or value.binaryPath:find("%z"))
    then
      fail("binaryPath must be a string without NUL bytes")
    end
  else
    fail('request must be "launch" or "attach"')
  end
end

return M
