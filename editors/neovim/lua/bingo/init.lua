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
  adapter = nil,
  previous_adapter = nil,
  configurations = {},
  listeners = {},
  generation = 0,
  sessions = setmetatable({}, { __mode = "k" }),
}

local default_configuration_names = {
  ["bingo: Debug Go package"] = true,
  ["bingo: Launch binary"] = true,
  ["bingo: Attach to process"] = true,
  ["bingo: Join session"] = true,
}

local function notify(message, level)
  vim.notify(message, level, { title = "bingo" })
end

local function prompt(label, default, completion)
  local ok, value = pcall(vim.fn.input, label, default or "", completion)
  if ok then
    return value
  end
  if not tostring(value):find("Vim:Interrupt", 1, true) then
    notify("prompt failed: " .. tostring(value), vim.log.levels.ERROR)
  end
  return nil
end

local function ensure_listener(dap, stage, name)
  dap.listeners[stage][name] = dap.listeners[stage][name] or {}
  return dap.listeners[stage][name]
end

local function remove_session(dap_session)
  local owned = state.sessions[dap_session]
  if owned and dap_session.on_close.bingo == owned.callback then
    dap_session.on_close.bingo = owned.previous
  end
  state.sessions[dap_session] = nil
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

local function register_session_listeners(dap, options)
  local generation = state.generation
  local function remove_current(dap_session)
    if generation == state.generation then
      remove_session(dap_session)
    end
  end
  local event_key = "event_" .. session_event.event_name
  local event_listener = function(dap_session, body)
    if generation ~= state.generation or state.sessions[dap_session] == nil then
      return
    end
    local announcement, decode_error = session_event.decode(body)
    if announcement == nil then
      notify("ignored invalid bingo session event: " .. decode_error, vim.log.levels.WARN)
      return
    end
    local prior = state.session_ids[dap_session]
    if prior then
      if prior ~= announcement.session_id then
        notify("ignored conflicting bingo session announcement", vim.log.levels.WARN)
      end
      return
    end

    state.session_ids[dap_session] = announcement.session_id
    state.current_session = announcement.session_id
    if options.notify_session then
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
  local on_session = function(_, new_session)
    if
      generation ~= state.generation
      or new_session == nil
      or new_session.config == nil
      or new_session.config.type ~= "bingo"
    then
      return
    end
    if state.sessions[new_session] then
      return
    end
    new_session.on_close = new_session.on_close or {}
    local previous = new_session.on_close.bingo
    local on_close = function(closed_session)
      vim.schedule(function()
        remove_current(closed_session)
      end)
      if previous then
        previous(closed_session)
      end
    end
    state.sessions[new_session] = { callback = on_close, previous = previous }
    new_session.on_close.bingo = on_close
  end
  local listeners = {
    event_key = event_key,
    event = event_listener,
    terminated = remove_current,
    exited = remove_current,
    on_session = on_session,
    previous = {
      event = ensure_listener(dap, "before", event_key).bingo,
      terminated = ensure_listener(dap, "before", "event_terminated").bingo,
      exited = ensure_listener(dap, "before", "event_exited").bingo,
      on_session = dap.listeners.on_session.bingo,
    },
  }
  state.listeners = listeners

  ensure_listener(dap, "before", event_key).bingo = event_listener
  ensure_listener(dap, "before", "event_terminated").bingo = remove_current
  ensure_listener(dap, "before", "event_exited").bingo = remove_current
  dap.listeners.on_session.bingo = on_session

  return listeners
end

local function required_prompt(dap, label, default, completion)
  local value = prompt(label, default, completion)
  if type(value) ~= "string" or value:match("^%s*$") then
    return dap.ABORT
  end
  return value
end

local function default_configurations(dap)
  return {
    {
      name = "bingo: Debug Go package",
      type = "bingo",
      request = "launch",
      mode = "debug",
      program = config.package_directory,
      args = {},
      env = {},
      stopOnEntry = false,
    },
    {
      name = "bingo: Launch binary",
      type = "bingo",
      request = "launch",
      mode = "exec",
      program = function()
        return required_prompt(
          dap,
          "Path to executable: ",
          vim.fn.getcwd() .. "/",
          "file"
        )
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
        local pid = tonumber(prompt("Process ID: "))
        if type(pid) ~= "number" or pid % 1 ~= 0 or pid <= 0 or pid > 2147483647 then
          return dap.ABORT
        end
        return pid
      end,
      binaryPath = function()
        local value = prompt("Path to executable (optional): ", "", "file")
        return value == nil and dap.ABORT or value
      end,
      stopOnEntry = true,
    },
    {
      name = "bingo: Join session",
      type = "bingo",
      request = "attach",
      session = function()
        return required_prompt(dap, "Managed bingo session ID: ")
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
  local added = {}
  for _, item in ipairs(default_configurations(dap)) do
    if not present[item.name] then
      dap.configurations.go[#dap.configurations.go + 1] = item
      added[#added + 1] = item
    end
  end
  return added
end

local function remove_configurations(dap)
  local go_configurations = dap.configurations.go
  if type(go_configurations) ~= "table" then
    state.configurations = {}
    return
  end
  local owned = {}
  for _, item in ipairs(state.configurations) do
    owned[item] = true
  end
  for index = #go_configurations, 1, -1 do
    if owned[go_configurations[index]] then
      table.remove(go_configurations, index)
    end
  end
  state.configurations = {}
end

local function remove_listener(dap, stage, name, callback, previous)
  local stage_listeners = dap.listeners[stage]
  if type(stage_listeners) ~= "table" then
    return
  end
  local listeners = rawget(stage_listeners, name)
  if listeners ~= nil and listeners.bingo == callback then
    listeners.bingo = previous
  end
end

local function unregister_dap()
  local dap = state.dap
  if dap == nil then
    return
  end

  remove_configurations(dap)
  if state.adapter ~= nil and dap.adapters.bingo == state.adapter then
    dap.adapters.bingo = state.previous_adapter
  end
  local listeners = state.listeners
  if listeners.event_key ~= nil then
    remove_listener(dap, "before", listeners.event_key, listeners.event, listeners.previous.event)
    remove_listener(dap, "before", "event_terminated", listeners.terminated, listeners.previous.terminated)
    remove_listener(dap, "before", "event_exited", listeners.exited, listeners.previous.exited)
  end
  local on_session = dap.listeners.on_session
  if listeners.on_session ~= nil and type(on_session) == "table"
    and on_session.bingo == listeners.on_session
  then
    on_session.bingo = listeners.previous.on_session
  end

  state.adapter = nil
  state.previous_adapter = nil
  state.listeners = {}
end

local function clear_setup()
  state.generation = state.generation + 1
  for dap_session in pairs(state.sessions) do
    remove_session(dap_session)
  end
  state.current_session = nil
  if state.manager ~= nil then
    state.manager:dispose()
  end
  unregister_dap()
  state.manager = nil
  state.dap = nil
  state.options = nil
end

function M.setup(options)
  if vim.fn.has("nvim-0.11.7") ~= 1 then
    error("bingo requires Neovim 0.11.7 or newer")
  end

  local dap = require("dap")
  local normalized = config.normalize(options)
  local manager = server.new(normalized)
  clear_setup()

  state.options = normalized
  state.manager = manager
  state.dap = dap
  state.previous_adapter = dap.adapters.bingo
  local generation = state.generation

  local adapter = function(callback, debug_config)
    if generation ~= state.generation then
      return
    end
    local valid, prepared = pcall(config.prepare_request, debug_config, normalized)
    if not valid then
      notify(tostring(prepared), vim.log.levels.ERROR)
      return
    end
    debug_config.program, debug_config.cwd = prepared.program, prepared.cwd
    manager:ensure(debug_config, function(error_message, endpoint)
      if generation ~= state.generation then
        return
      end
      if error_message ~= nil then
        notify(error_message, vim.log.levels.ERROR)
        return
      end
      local ok, callback_error = pcall(callback, {
        type = "server",
        host = endpoint.host,
        port = endpoint.port,
        options = {
          -- nvim-dap's watchdog includes the launch response, not just initialize.
          initialize_timeout_sec = debug_config.mode == "debug" and 130 or 10,
          disconnect_timeout_sec = 5,
        },
      })
      if not ok then
        notify("bingo adapter callback failed: " .. tostring(callback_error), vim.log.levels.ERROR)
      end
    end)
  end
  state.adapter = adapter
  dap.adapters.bingo = adapter

  local listeners_ok, listeners = pcall(register_session_listeners, dap, normalized)
  if not listeners_ok then
    clear_setup()
    error(listeners, 0)
  end
  state.listeners = listeners
  if state.options.configurations then
    state.configurations = add_configurations(dap)
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

local function run(configuration)
  local ok, prepared = pcall(config.prepare_request, configuration, state.options)
  if not ok then
    notify(tostring(prepared), vim.log.levels.ERROR)
    return
  end
  state.dap.run(prepared)
end

function M.debug(directory)
  ensure_setup()
  run({
    name = "bingo: Debug Go package",
    type = "bingo",
    request = "launch",
    mode = "debug",
    program = directory == nil and config.package_directory() or directory,
    args = {},
    env = {},
    stopOnEntry = false,
  })
end

function M.launch(program)
  ensure_setup()
  if program == nil then
    program = prompt("Path to executable: ", vim.fn.getcwd() .. "/", "file")
    if program == nil or program:match("^%s*$") then
      return
    end
  end
  program = require_non_empty(program, "executable path")
  if program == nil then
    return
  end
  run({
    name = "bingo: " .. vim.fs.basename(program),
    type = "bingo",
    request = "launch",
    mode = "exec",
    program = program,
    args = {},
    env = {},
    stopOnEntry = true,
  })
end

function M.attach(pid, binary_path)
  ensure_setup()
  if pid == nil then
    local value = prompt("Process ID: ")
    if value == nil or value:match("^%s*$") then
      return
    end
    pid = tonumber(value)
  end
  if type(pid) ~= "number" or pid % 1 ~= 0 or pid <= 0 or pid > 2147483647 then
    notify("a positive process ID is required", vim.log.levels.ERROR)
    return
  end
  if binary_path == nil then
    binary_path = prompt(
      "Path to executable (optional): ",
      "",
      "file"
    )
    if binary_path == nil then
      return
    end
  end
  run({
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
  if session_id == nil then
    session_id = prompt("Managed bingo session ID: ")
    if session_id == nil or session_id:match("^%s*$") then
      return
    end
  end
  session_id = require_non_empty(session_id, "managed session ID")
  if session_id == nil then
    return
  end
  run({
    name = "bingo: Join " .. session_id,
    type = "bingo",
    request = "attach",
    session = session_id,
  })
end

function M.session_id()
  return state.current_session
end

function M.server_info()
  return server.inspect(state.options or config.normalize())
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
  clear_setup()
  state.current_session = nil
  state.session_ids = setmetatable({}, { __mode = "k" })
end

return M
