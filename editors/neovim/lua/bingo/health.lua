local M = {}

M.service = "bingo"
M.management_api_version = 1
M.wire_protocol_version = "1.2"
M.session_event_version = 1

local maximum_response_bytes = 64 * 1024

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
    host, port_text = value:match("^(.*):(%d+)$")
  end
  local port = tonumber(port_text)
  if host == nil or host == "" or port == nil or port % 1 ~= 0 or port < 1 or port > 65535 then
    return nil
  end
  return { host = host, port = port }
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
  if type(decoded.instanceId) ~= "string" or decoded.instanceId == "" then
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

function M.parse_http_response(raw)
  local headers, body = raw:match("^(.-)\r\n\r\n(.*)$")
  if headers == nil then
    return nil, "health endpoint returned an incomplete HTTP response"
  end
  local status = tonumber(headers:match("^HTTP/%d+%.%d+%s+(%d%d%d)"))
  if status == nil then
    return nil, "health endpoint returned an invalid HTTP status line"
  end
  return { status = status, body = body }
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
  vim.health.info("Server compatibility is checked when a bingo debug session starts")
end

function M.probe(endpoint, expected_dap, timeout_ms, callback, dependencies)
  local deps = dependencies or default_dependencies()
  local uv = deps.uv
  local tcp = uv.new_tcp()
  local timer = uv.new_timer()
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

  local function finish_response()
    local response, parse_error = M.parse_http_response(table.concat(chunks))
    if response == nil then
      finish({ kind = "transportError", error = parse_error })
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

  timer:start(timeout_ms, 0, function()
    finish({
      kind = "transportError",
      error = string.format("health request timed out after %dms", timeout_ms),
    })
  end)

  tcp:connect(endpoint.host, endpoint.port, function(connect_error)
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
      endpoint.host,
      ":",
      tostring(endpoint.port),
      "\r\n",
      "Accept: application/json\r\n",
      "Cache-Control: no-cache\r\n",
      "Connection: close\r\n\r\n",
    })
    tcp:write(request, function(write_error)
      if finished then
        return
      end
      if write_error ~= nil then
        finish({ kind = "transportError", error = tostring(write_error) })
        return
      end
      tcp:read_start(function(read_error, chunk)
        if finished then
          return
        end
        if read_error ~= nil then
          finish({ kind = "transportError", error = tostring(read_error) })
          return
        end
        if chunk == nil then
          finish_response()
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
      end)
    end)
  end)
end

return M
