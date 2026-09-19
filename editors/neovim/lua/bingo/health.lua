local config = require("bingo.config")
local http = require("bingo.http")
local M = {}

M.service = "bingo"
M.management_api_version = 1
M.wire_protocol_version = "1.4"
M.session_event_version = 1
M.source_launch_version = 1

local maximum_response_bytes = http.maximum_response_bytes

local function record(value)
  return type(value) == "table"
end

local function incompatible(reason)
  return { kind = "incompatible", reason = reason }
end

local function parse_address(value)
  if type(value) ~= "string" then
    return nil
  end

  local host
  local port_text
  if value:sub(1, 1) == "[" then
    host, port_text = value:match("^%[([^]]+)%]:(%d+)$")
  else
    host, port_text = value:match("^([^:]*):(%d+)$")
  end
  local port = tonumber(port_text)
  if host == nil or host == "" or port == nil or port % 1 ~= 0 or port < 1 or port > 65535 then
    return nil
  end
  local ok, resolved = pcall(config.resolve, {
    serverMode = "connectOnly", dapHost = host, dapPort = port,
  })
  return ok and resolved.dap or nil
end

local function wildcard(host)
  return host == "" or host == "0.0.0.0" or host == "::"
end

function M.validate(status_code, decoded, expected_dap)
  if status_code ~= 200 then
    return incompatible(string.format("health endpoint returned HTTP %d", status_code))
  end
  if not record(decoded) then
    return incompatible("health response must be an object")
  end
  if decoded.service ~= M.service then
    return incompatible(
      string.format(
        'health service identity is %s, expected "%s"',
        tostring(decoded.service),
        M.service
      )
    )
  end
  if decoded.managementApiVersion ~= M.management_api_version then
    return incompatible(
      string.format(
        "management API version is %s, expected %d",
        tostring(decoded.managementApiVersion),
        M.management_api_version
      )
    )
  end
  if decoded.wireProtocolVersion ~= M.wire_protocol_version then
    return incompatible(
      string.format(
        "wire protocol version is %s, expected %s",
        tostring(decoded.wireProtocolVersion),
        M.wire_protocol_version
      )
    )
  end
  if type(decoded.instanceId) ~= "string" or decoded.instanceId:match("^%s*$") then
    return incompatible("health response has no instanceId")
  end
  if not record(decoded.dap) or decoded.dap.enabled ~= true then
    return incompatible("bingo DAP listener is not enabled")
  end
  if decoded.dap.sessionEventVersion ~= M.session_event_version then
    return incompatible(
      string.format(
        "DAP session event version is %s, expected %d",
        tostring(decoded.dap.sessionEventVersion),
        M.session_event_version
      )
    )
  end
  if decoded.dap.sourceLaunchVersion ~= M.source_launch_version then
    return incompatible(
      string.format(
        "DAP source launch version is %s, expected %d; update the bingo server",
        tostring(decoded.dap.sourceLaunchVersion),
        M.source_launch_version
      )
    )
  end

  local advertised = parse_address(decoded.dap.address)
  if advertised == nil then
    return incompatible(
      "health DAP address is invalid: " .. tostring(decoded.dap.address)
    )
  end
  if advertised.port ~= expected_dap.port then
    return incompatible(
      string.format(
        "health DAP port is %d, expected %d",
        advertised.port,
        expected_dap.port
      )
    )
  end
  if not wildcard(advertised.host) and advertised.host ~= expected_dap.host then
    return incompatible(
      string.format(
        "health DAP host is %s, expected %s",
        advertised.host,
        expected_dap.host
      )
    )
  end

  return {
    kind = "compatible",
    health = {
      instance_id = decoded.instanceId,
      dap_address = decoded.dap.address,
    },
  }
end

function M.parse_http_response(raw, eof)
  return http.parse(raw, eof == nil or eof)
end

local function default_dependencies()
  return {
    uv = vim.uv or vim.loop,
    schedule = vim.schedule,
    decode = vim.json.decode,
  }
end

local function refused(error_message)
  return type(error_message) == "string"
    and error_message:find("ECONNREFUSED", 1, true) ~= nil
end

function M.check()
  vim.health.start("bingo")
  if vim.fn.has("nvim-0.11.7") == 1 then
    vim.health.ok("Neovim 0.11.7 or newer is available")
  else
    vim.health.error("bingo requires Neovim 0.11.7 or newer")
  end

  local dap_ok = pcall(require, "dap")
  if dap_ok then
    vim.health.ok("nvim-dap is available")
  else
    vim.health.error("nvim-dap is required")
  end

  local ok, info = pcall(function() return require("bingo").server_info() end)
  if not ok then
    vim.health.error("Cannot resolve bingo configuration: " .. tostring(info))
    return
  end
  if info.mode == "connectOnly" then
    vim.health.info("connectOnly: DAP " .. info.dap .. "; no local binary, health probe, or autostart")
    vim.health.info("Source packages and Go toolchain must be available on the server, not this editor")
    return
  end
  if info.platform_supported then
    vim.health.ok("Native server platform is supported")
  else
    vim.health.error("Autostart supports only linux/amd64 and darwin/arm64; use connectOnly for an existing server")
  end
  if info.binary then
    vim.health.ok("Server binary: " .. info.binary)
  else
    vim.health.warn(info.binary_error)
    vim.health.info("Prepare from the monorepo: bash " .. vim.fn.shellescape(info.prepare_script))
  end
  local go = vim.fn.exepath("go")
  if go ~= "" then
    vim.health.ok("Go for local source launches: " .. go)
  else
    vim.health.warn("Go is not on PATH; :BingoDebug needs Go on the server's PATH. Binary launch and attach do not compile targets")
  end
  if info.platform == "Darwin" and (vim.fn.executable("codesign") ~= 1 or vim.fn.executable("xcrun") ~= 1) then
    vim.health.warn("Building the macOS server requires Xcode Command Line Tools (xcode-select --install) and codesign; prepared releases are already signed")
  end
  vim.health.info("Management " .. info.management .. "; DAP " .. info.dap)
  vim.health.info("Server compatibility is checked when a bingo debug session starts")
end

function M.probe(endpoint, expected_dap, timeout_ms, callback, dependencies)
  local deps = dependencies or default_dependencies()
  local uv = deps.uv
  local tcp
  local timer
  local chunks = {}
  local size = 0
  local finished = false

  local function close_handles()
    if timer ~= nil and not timer:is_closing() then
      timer:stop()
      timer:close()
    end
    if tcp ~= nil and not tcp:is_closing() then
      tcp:close()
    end
  end

  local function finish(result)
    if finished then
      return
    end
    finished = true
    close_handles()
    deps.schedule(function()
      callback(result)
    end)
  end

  local function finish_response(eof)
    local response, parse_error = M.parse_http_response(table.concat(chunks), eof)
    if response == nil then
      if parse_error then
        finish({ kind = "transportError", error = parse_error })
      end
      return
    end
    if response.status ~= 200 then
      finish(M.validate(response.status, nil, expected_dap))
      return
    end

    local ok, decoded = pcall(deps.decode, response.body)
    if not ok then
      finish({
        kind = "incompatible",
        reason = "health endpoint did not return JSON",
      })
      return
    end
    finish(M.validate(response.status, decoded, expected_dap))
  end

  local function invoke(operation, callback, ...)
    local ok, result, err = pcall(callback, ...)
    if not ok or err ~= nil then
      finish({ kind = "transportError", error = operation .. ": " .. tostring(ok and err or result) })
      return nil
    end
    return result
  end

  tcp = invoke("create health TCP socket", function()
    local handle, err = uv.new_tcp()
    return handle, err or (not handle and "no TCP handle" or nil)
  end)
  if not finished then
    timer = invoke("create health timer", function()
      local handle, err = uv.new_timer()
      return handle, err or (not handle and "no timer handle" or nil)
    end)
  end
  local function on_timeout()
    finish({
      kind = "transportError",
      error = string.format("health request timed out after %dms", timeout_ms),
    })
  end

  local function on_read(read_error, chunk)
    if finished then
      return
    end
    if read_error ~= nil then
      finish({ kind = "transportError", error = tostring(read_error) })
      return
    end
    if chunk == nil then
      finish_response(true)
      return
    end
    size = size + #chunk
    if size > maximum_response_bytes then
      finish({
        kind = "transportError",
        error = "bingo health response exceeded 64 KiB",
      })
      return
    end
    chunks[#chunks + 1] = chunk
    finish_response(false)
  end

  local function on_write(write_error)
    if finished then
      return
    end
    if write_error ~= nil then
      finish({ kind = "transportError", error = tostring(write_error) })
      return
    end
    invoke("read health response", tcp.read_start, tcp, on_read)
  end

  local function on_connect(connect_error)
    if finished then
      return
    end
    if connect_error ~= nil then
      if refused(connect_error) then
        finish({ kind = "absent" })
      else
        finish({ kind = "transportError", error = tostring(connect_error) })
      end
      return
    end

    local request = table.concat({
      "GET /api/health HTTP/1.1\r\n",
      "Host: ",
      config.endpoint(endpoint),
      "\r\n",
      "Accept: application/json\r\n",
      "Cache-Control: no-cache\r\n",
      "Connection: close\r\n\r\n",
    })
    invoke("write health request", tcp.write, tcp, request, on_write)
  end

  if not finished then
    invoke("start health timer", timer.start, timer, timeout_ms, 0, on_timeout)
  end
  if not finished then
    invoke("connect health socket", tcp.connect, tcp, endpoint.host, endpoint.port, on_connect)
  end
  return function()
    finish({ kind = "transportError", error = "health request cancelled" })
  end
end

return M
