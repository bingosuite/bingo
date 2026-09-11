local M = {}
M.maximum_response_bytes = 64 * 1024
local maximum_header_bytes = 8 * 1024

local function fields(text)
  local result = {}
  for line in (text .. "\r\n"):gmatch("(.-)\r\n") do
    if line ~= "" then
      local name, value = line:match("^([!#$%%&'*+.^_`|~%w%-]+):[ \t]*(.-)[ \t]*$")
      if not name or value:find("%z") or value:find("[\001-\008\010-\031\127]") then
        return nil, "invalid HTTP header"
      end
      name = name:lower()
      if result[name] and (name == "content-length" or name == "transfer-encoding") then
        return nil, "duplicate HTTP framing header"
      end
      result[name] = value
    end
  end
  return result
end

-- A nil error means more bytes are needed, not that a partial response is valid.
function M.parse(raw, eof)
  if #raw > M.maximum_response_bytes then
    return nil, "bingo health response exceeded 64 KiB"
  end
  local boundary = raw:find("\r\n\r\n", 1, true)
  if (boundary and boundary + 3 > maximum_header_bytes)
    or (not boundary and #raw > maximum_header_bytes)
  then
    return nil, "health HTTP headers exceeded 8 KiB"
  end
  if not boundary then
    return nil, eof and "incomplete HTTP headers" or nil
  end
  local head = raw:sub(1, boundary - 1)
  local first_end = head:find("\r\n", 1, true)
  local first = first_end and head:sub(1, first_end - 1) or head
  local status = first:match("^HTTP/1%.[01] (%d%d%d) [^\r\n]*$")
  if not status or tonumber(status) < 100 then
    return nil, "invalid HTTP status line"
  end
  local headers, message = fields(first_end and head:sub(first_end + 2) or "")
  if not headers then
    return nil, message
  end
  local body = raw:sub(boundary + 4)
  local length, transfer = headers["content-length"], headers["transfer-encoding"]
  if length and transfer then
    return nil, "conflicting HTTP framing headers"
  end
  if transfer then
    if transfer:lower() ~= "chunked" then
      return nil, "unsupported HTTP transfer encoding"
    end
    local chunks, offset = {}, 1
    while true do
      local line_end = body:find("\r\n", offset, true)
      if not line_end then
        return nil, eof and "incomplete HTTP chunk size" or nil
      end
      local size_line = body:sub(offset, line_end - 1)
      local hex, extension = size_line:match("^(%x+)(.*)$")
      if not hex or (extension ~= "" and not extension:match("^;[^\r\n%z]+$")) then
        return nil, "invalid HTTP chunk size"
      end
      local size = tonumber(hex, 16)
      if not size or size > M.maximum_response_bytes then
        return nil, "HTTP chunk exceeded 64 KiB"
      end
      offset = line_end + 2
      if size == 0 then
        local trailer_end
        if body:sub(offset, offset + 1) == "\r\n" then
          trailer_end = offset
        else
          local last = body:find("\r\n\r\n", offset, true)
          local trailer_bytes = last and last - offset + 4 or #body - offset + 1
          if trailer_bytes > maximum_header_bytes then
            return nil, "HTTP trailers exceeded 8 KiB"
          end
          if not last then
            return nil, eof and "incomplete HTTP trailers" or nil
          end
          local trailers, trailer_error = fields(body:sub(offset, last - 1))
          if not trailers then
            return nil, trailer_error
          end
          if trailers["content-length"] or trailers["transfer-encoding"] then
            return nil, "HTTP trailers contain framing headers"
          end
          trailer_end = last + 2
        end
        if trailer_end + 1 ~= #body then
          return nil, "trailing bytes after HTTP response"
        end
        return { status = tonumber(status), body = table.concat(chunks) }
      end
      if #body < offset + size + 1 then
        return nil, eof and "incomplete HTTP chunk" or nil
      end
      if body:sub(offset + size, offset + size + 1) ~= "\r\n" then
        return nil, "invalid HTTP chunk terminator"
      end
      chunks[#chunks + 1] = body:sub(offset, offset + size - 1)
      offset = offset + size + 2
    end
  end
  if length then
    if not length:match("^%d+$") then
      return nil, "invalid HTTP Content-Length"
    end
    length = tonumber(length)
    if length > M.maximum_response_bytes then
      return nil, "HTTP Content-Length exceeded 64 KiB"
    end
    if #body < length then
      return nil, eof and "incomplete HTTP body" or nil
    end
    if #body > length then
      return nil, "trailing bytes after HTTP response"
    end
  elseif not eof then
    return nil
  end
  return { status = tonumber(status), body = body }
end

return M
