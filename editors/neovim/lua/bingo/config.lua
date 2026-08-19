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
  if type(value) ~= "string" or value:match("^%s*$") then
    fail(name .. " must be a non-empty string")
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
  options = options or {}
  if type(options) ~= "table" then
    fail("setup options must be a table")
  end
  local server = options.server or {}
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
  result.server.management_host = string_value(
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
  result.server.dap_host = string_value(
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

local function debug_string(debug_config, key, fallback)
  local value = debug_config[key]
  return string_value(value, fallback, key)
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

  return {
    mode = resolved_mode,
    management = {
      host = debug_string(
        debug_config,
        "managementHost",
        server.management_host
      ),
      port = debug_integer(
        debug_config,
        "managementPort",
        server.management_port,
        1,
        65535
      ),
    },
    dap = {
      host = debug_string(debug_config, "dapHost", server.dap_host),
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
end

function M.endpoint(endpoint)
  return string.format("%s:%d", endpoint.host, endpoint.port)
end

return M
