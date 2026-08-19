local M = {}

M.event_name = "bingo/session/v1"
M.event_version = 1

function M.decode(body)
  if type(body) ~= "table" then
    return nil, "bingo session event body must be an object"
  end
  local count = 0
  for key in pairs(body) do
    if key ~= "version" and key ~= "sessionId" then
      return nil, "bingo session event body has unexpected fields"
    end
    count = count + 1
  end
  if count ~= 2 then
    return nil, "bingo session event body has unexpected fields"
  end
  if body.version ~= M.event_version then
    return nil, "unsupported bingo session event version " .. tostring(body.version)
  end
  if
    type(body.sessionId) ~= "string"
    or #body.sessionId == 0
    or #body.sessionId > 128
    or body.sessionId:match("^[A-Za-z0-9][A-Za-z0-9._-]*$") == nil
  then
    return nil, "bingo session event has an invalid sessionId"
  end
  return { session_id = body.sessionId }
end

return M
