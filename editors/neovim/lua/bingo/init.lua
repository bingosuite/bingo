local config = require("bingo.config")
local server = require("bingo.server")
local session_event = require("bingo.session")

local M = {}
local state = {
  dap = nil,
  manager = nil,
  options = nil,
  current_session = nil,
  session_ids = setmetatable({}, { __mode = "k" }),
}

local default_configuration_names = {
  ["bingo: Launch binary"] = true,
  ["bingo: Attach to process"] = true,
  ["bingo: Join session"] = true,
}

local function notify(message, level)
  vim.notify(message, level, { title = "bingo" })
end

local function prompt(label, default, completion)
  return vim.fn.input(label, default or "", completion)
end

local function ensure_listener(dap, stage, name)
  dap.listeners[stage][name] = dap.listeners[stage][name] or {}
  return dap.listeners[stage][name]
end

local function remove_session(dap_session)
  local removed = state.session_ids[dap_session]
  state.session_ids[dap_session] = nil
  if state.current_session == removed then
    state.current_session = nil
    for _, id in pairs(state.session_ids) do
      state.current_session = id
      break
    end
  end
end

local function register_session_listeners(dap)
  local event_key = "event_" .. session_event.event_name
  ensure_listener(dap, "before", event_key).bingo = function(dap_session, body)
    local announcement, decode_error = session_event.decode(body)
    if announcement == nil then
      notify("ignored invalid bingo session event: " .. decode_error, vim.log.levels.WARN)
      return
    end

    state.session_ids[dap_session] = announcement.session_id
    state.current_session = announcement.session_id
    if state.options.notify_session then
      notify("managed session " .. announcement.session_id, vim.log.levels.INFO)
    end
    vim.api.nvim_exec_autocmds("User", {
      pattern = "BingoSession",
      modeline = false,
      data = {
        session_id = announcement.session_id,
      },
    })
  end

  ensure_listener(dap, "before", "event_terminated").bingo = remove_session
  ensure_listener(dap, "before", "event_exited").bingo = remove_session
  dap.listeners.on_session.bingo = function(_, new_session)
    if
      new_session == nil
      or new_session.config == nil
      or new_session.config.type ~= "bingo"
    then
      return
    end
    new_session.on_close = new_session.on_close or {}
    new_session.on_close.bingo = function(closed_session)
      vim.schedule(function()
        remove_session(closed_session)
      end)
    end
  end
end

local function default_configurations()
  return {
    {
      name = "bingo: Launch binary",
      type = "bingo",
      request = "launch",
      program = function()
        return prompt("Path to executable: ", vim.fn.getcwd() .. "/", "file")
      end,
      args = {},
      env = {},
      stopOnEntry = true,
    },
    {
      name = "bingo: Attach to process",
      type = "bingo",
      request = "attach",
      pid = function()
        return tonumber(prompt("Process ID: "))
      end,
      binaryPath = function()
        return prompt("Path to executable (optional): ", vim.fn.getcwd() .. "/", "file")
      end,
      stopOnEntry = true,
    },
    {
      name = "bingo: Join session",
      type = "bingo",
      request = "attach",
      session = function()
        return prompt("Managed bingo session ID: ")
      end,
    },
  }
end

local function add_configurations(dap)
  dap.configurations.go = dap.configurations.go or {}
  local present = {}
  for _, item in ipairs(dap.configurations.go) do
    if item.type == "bingo" and default_configuration_names[item.name] then
      present[item.name] = true
    end
  end
  for _, item in ipairs(default_configurations()) do
    if not present[item.name] then
      dap.configurations.go[#dap.configurations.go + 1] = item
    end
  end
end

function M.setup(options)
  if vim.fn.has("nvim-0.11.7") ~= 1 then
    error("bingo requires Neovim 0.11.7 or newer")
  end

  local dap = require("dap")
  if state.manager ~= nil then
    state.manager:dispose()
  end
  state.options = config.normalize(options)
  state.manager = server.new(state.options)
  state.dap = dap

  dap.adapters.bingo = function(callback, debug_config)
    state.manager:ensure(debug_config, function(error_message, endpoint)
      if error_message ~= nil then
        notify(error_message, vim.log.levels.ERROR)
        return
      end
      callback({
        type = "server",
        host = endpoint.host,
        port = endpoint.port,
        options = {
          initialize_timeout_sec = 10,
          disconnect_timeout_sec = 5,
        },
      })
    end)
  end

  register_session_listeners(dap)
  if state.options.configurations then
    add_configurations(dap)
  end
  return M
end

local function ensure_setup()
  if state.dap == nil then
    M.setup()
  end
end

local function require_non_empty(value, label)
  if type(value) ~= "string" or value:match("^%s*$") then
    notify(label .. " is required", vim.log.levels.ERROR)
    return nil
  end
  return value
end

function M.launch(program)
  ensure_setup()
  program = program or prompt("Path to executable: ", vim.fn.getcwd() .. "/", "file")
  program = require_non_empty(program, "executable path")
  if program == nil then
    return
  end
  state.dap.run({
    name = "bingo: " .. vim.fs.basename(program),
    type = "bingo",
    request = "launch",
    program = program,
    args = {},
    env = {},
    stopOnEntry = true,
  })
end

function M.attach(pid, binary_path)
  ensure_setup()
  pid = pid or tonumber(prompt("Process ID: "))
  if type(pid) ~= "number" or pid % 1 ~= 0 or pid <= 0 then
    notify("a positive process ID is required", vim.log.levels.ERROR)
    return
  end
  if binary_path == nil then
    binary_path = prompt(
      "Path to executable (optional): ",
      vim.fn.getcwd() .. "/",
      "file"
    )
  end
  state.dap.run({
    name = "bingo: Attach " .. tostring(pid),
    type = "bingo",
    request = "attach",
    pid = pid,
    binaryPath = binary_path,
    stopOnEntry = true,
  })
end

function M.join(session_id)
  ensure_setup()
  session_id = session_id or prompt("Managed bingo session ID: ")
  session_id = require_non_empty(session_id, "managed session ID")
  if session_id == nil then
    return
  end
  state.dap.run({
    name = "bingo: Join " .. session_id,
    type = "bingo",
    request = "attach",
    session = session_id,
  })
end

function M.session_id()
  return state.current_session
end

function M.show_session()
  local id = M.session_id()
  if id == nil then
    notify("no active bingo managed session", vim.log.levels.INFO)
    return
  end
  notify("managed session " .. id, vim.log.levels.INFO)
end

function M.dispose()
  if state.manager ~= nil then
    state.manager:dispose()
  end
  state.manager = nil
  state.dap = nil
  state.current_session = nil
  state.session_ids = setmetatable({}, { __mode = "k" })
end

return M
