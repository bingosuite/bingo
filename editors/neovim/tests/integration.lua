local root = assert(vim.env.BINGO_NVIM_SMOKE_ROOT, "run scripts/integration.sh")
local work = assert(vim.env.BINGO_NVIM_SMOKE_WORK, "run scripts/integration.sh")
local uv = vim.uv
work = assert(uv.fs_realpath(work))
local package_dir = work .. "/source package"
local source = package_dir .. "/main.go"
local server_log = work .. "/server.log"
local target_path
local retain_observer = vim.env.BINGO_NVIM_SMOKE_RETAIN_OBSERVER ~= "0"
local server_handle, server_pid, server_exit, target_pid
local dap, bingo
local errors, observations = {}, {}
local counts = {
  announcements = 0,
  user_events = 0,
  stopped = 0,
  terminate_intents = 0,
  terminate_responses = 0,
  terminate_callbacks = 0,
  close_callbacks = 0,
  terminated_events = 0,
}

vim.opt.runtimepath = { root .. "/editors/neovim", work .. "/nvim-dap", vim.env.VIMRUNTIME }
vim.opt.packpath = { vim.env.VIMRUNTIME }
vim.opt.swapfile = false
vim.opt.shadafile = "NONE"

local function note(message)
  observations[#observations + 1] = message
  print("SMOKE: " .. message)
end

local function guarded(callback)
  return function(...)
    local args = { ... }
    local count = select("#", ...)
    local ok, message = xpcall(function()
      callback(unpack(args, 1, count))
    end, debug.traceback)
    if not ok then
      errors[#errors + 1] = message
    end
  end
end

vim.notify = function(message, level)
  if level == vim.log.levels.ERROR then
    errors[#errors + 1] = tostring(message)
  end
  note("notification: " .. tostring(message))
end

local function check_errors()
  if vim.v.errmsg ~= "" then
    error("Neovim asynchronous error: " .. vim.v.errmsg, 0)
  end
  assert(#errors == 0, table.concat(errors, "\n"))
end

local function wait_for(description, predicate, timeout)
  local ready = vim.wait(timeout or 15000, function()
    return #errors > 0 or vim.v.errmsg ~= "" or predicate()
  end, 20)
  check_errors()
  assert(ready and predicate(), "timed out: " .. description)
  note(description)
end

local function text(path)
  if vim.fn.filereadable(path) ~= 1 then
    return ""
  end
  return table.concat(vim.fn.readfile(path), "\n")
end

local function alive(pid)
  return pid ~= nil and uv.kill(pid, 0) == 0
end

local function executable(pid)
  if uv.os_uname().sysname == "Linux" then
    return uv.fs_readlink("/proc/" .. pid .. "/exe")
  end
  local result = vim.system({ "ps", "-p", tostring(pid), "-o", "comm=" }, { text = true }):wait(1000)
  if result.code == 0 then
    return vim.trim(result.stdout)
  end
end

local function owned_target(path)
  local prefix = work .. "/work/"
  return type(path) == "string" and path:sub(1, #prefix) == prefix
end

local management_port
local function http(path)
  local result = vim.system({
    "curl", "--fail", "--silent", "--show-error", "--noproxy", "*",
    "--max-time", "1", "http://127.0.0.1:" .. management_port .. path,
  }, { text = true }):wait(1500)
  if result.code ~= 0 then
    return nil, result.stderr
  end
  return vim.json.decode(result.stdout)
end

local function request(session, command, arguments)
  local completed, response, response_error
  session:request(command, arguments, guarded(function(err, body)
    response_error, response, completed = err, body, true
  end))
  wait_for("DAP " .. command .. " callback", function()
    return completed
  end)
  assert(response_error == nil, command .. ": " .. vim.inspect(response_error))
  assert(type(response) == "table", command .. " response missing")
  return response
end

local function session_count()
  return vim.tbl_count(dap.sessions())
end

local function smoke()
  assert(vim.fn.has("nvim-0.11.7") == 1, "requires Neovim 0.11.7 or newer")
  local log_fd = assert(uv.fs_open(server_log, "w", 384))
  local idle_ms = 5000
  local spawn_error
  server_handle, spawn_error = uv.spawn(assert(vim.env.BINGO_NVIM_SMOKE_SERVER), {
    args = { "-addr", "127.0.0.1:0", "-dap-addr", "127.0.0.1:0", "-idle-timeout", idle_ms .. "ms", "-v" },
    cwd = root,
    stdio = { nil, log_fd, log_fd },
  }, guarded(function(code, signal)
    server_exit = { code = code, signal = signal }
    if server_handle and not server_handle:is_closing() then
      server_handle:close()
    end
  end))
  uv.fs_close(log_fd)
  assert(server_handle, "server spawn failed: " .. tostring(spawn_error))
  server_pid = spawn_error
  note("spawned owned server pid=" .. server_pid)

  -- Port zero lets each listener claim its own endpoint without a
  -- reserve-close-rebind race against another test or a user's server.
  local dap_port
  wait_for("server bound private loopback endpoints", function()
    local output = text(server_log)
    management_port = output:match('msg="bingo server listening"[^\n]- addr=127%.0%.0%.1:(%d+)')
    dap_port = output:match('msg="dap server listening"[^\n]- addr=127%.0%.0%.1:(%d+)')
    return management_port ~= nil and dap_port ~= nil
  end, 4000)
  assert(management_port ~= dap_port, "HTTP and DAP endpoints overlap")
  assert(server_exit == nil and alive(server_pid), "server did not remain running")
  local health = assert(http("/api/health"))
  assert(health.service == "bingo" and health.sessionCount == 0, "unexpected initial health")
  assert(health.dap.sourceLaunchVersion == 1, "server does not support source launch version 1")
  assert(health.dap.address == "127.0.0.1:" .. dap_port, "health advertised wrong DAP endpoint")
  assert(health.managedIdleShutdown.timeoutMs == idle_ms, "server-owned idle shutdown disabled")
  note("health ready HTTP=" .. management_port .. " DAP=" .. dap_port .. " sessions=0")

  dap = require("dap")
  dap.set_log_level("TRACE")
  bingo = require("bingo").setup({
    configurations = false,
    notify_session = false,
    server = {
      mode = "connectOnly",
      management_host = "127.0.0.1",
      management_port = tonumber(management_port),
      dap_host = "127.0.0.1",
      dap_port = tonumber(dap_port),
    },
  })
  local announcement, announced_session, stopped
  local announced, attached, terminated = {}, {}, {}
  dap.listeners.before["event_bingo/session/v1"].native_smoke = guarded(function(session, body)
    counts.announcements = counts.announcements + 1
    assert(body.version == 1 and type(body.sessionId) == "string", "bad custom DAP event")
    if announcement == nil then
      announcement, announced_session = body.sessionId, session
    end
    assert(body.sessionId == announcement, "observer joined a different managed session")
    announced[session] = body.sessionId
    session.on_close.native_smoke = guarded(function(closed)
      assert(closed == session, "on_close received a foreign session")
      counts.close_callbacks = counts.close_callbacks + 1
    end)
  end)
  vim.api.nvim_create_autocmd("User", {
    pattern = "BingoSession",
    callback = guarded(function(event)
      counts.user_events = counts.user_events + 1
      assert(event.data.session_id == bingo.session_id(), "BingoSession/session_id mismatch")
    end),
  })
  dap.listeners.after.event_stopped.native_smoke = guarded(function(session, body)
    counts.stopped = counts.stopped + 1
    stopped = { session = session, body = body }
  end)
  dap.listeners.before.terminate.native_smoke = guarded(function(_, err)
    assert(err == nil, "terminate response failed: " .. vim.inspect(err))
    counts.terminate_responses = counts.terminate_responses + 1
  end)
  dap.listeners.before.attach.native_smoke = guarded(function(session, err)
    assert(err == nil, "join response failed: " .. vim.inspect(err))
    attached[session] = true
  end)
  dap.listeners.before.event_terminated.native_smoke = guarded(function(session)
    counts.terminated_events = counts.terminated_events + 1
    terminated[session] = (terminated[session] or 0) + 1
  end)

  local breakpoint_line
  for line, value in ipairs(vim.fn.readfile(source)) do
    if value:find("NVIM_SMOKE_BREAKPOINT", 1, true) then
      assert(breakpoint_line == nil, "ambiguous source breakpoint marker")
      breakpoint_line = line
    end
  end
  assert(breakpoint_line, "source breakpoint marker missing")
  vim.cmd.edit(vim.fn.fnameescape(source))
  vim.api.nvim_win_set_cursor(0, { breakpoint_line, 0 })
  dap.set_breakpoint()
  bingo.debug()

  wait_for("one real custom event and companion session announcement", function()
    return announcement ~= nil and bingo.session_id() == announcement
  end, 130000)
  assert(counts.announcements == 1 and counts.user_events == 1, "duplicate or missing session events")
  wait_for("source-built target reaches its breakpoint without an entry resume", function()
    target_pid = tonumber(text(vim.env.BINGO_NVIM_SMOKE_PID_FILE))
    return stopped ~= nil and stopped.body.reason == "breakpoint"
  end)
  local session = assert(dap.session(), "nvim-dap has no focused session")
  assert(session == announced_session and session_count() == 1, "nvim-dap live sessions() mismatch")
  assert(dap.sessions()[session.id] == session, "announced session absent from sessions()")
  local sessions = assert(http("/api/sessions"))
  assert(#sessions == 1 and sessions[1].id == announcement and sessions[1].state == "suspended",
    "management session does not match nvim-dap/companion")
  assert(sessions[1].clients == 1, "unexpected client or retained session")
  note("session=" .. announcement .. " nvim-dap live sessions=1 managed sessions=1")

  assert(session.config.mode == "debug" and session.config.program == package_dir
    and session.config.stopOnEntry == false, "no-argument source launch configuration mismatch")
  assert(counts.stopped == 1, "source launch exposed an entry stop")
  assert(target_pid and alive(target_pid), "target pid not live at source breakpoint")
  target_path = executable(target_pid)
  assert(owned_target(target_path), "target executable is not a server-owned private build: " .. tostring(target_path))
  assert(text(vim.env.BINGO_NVIM_SMOKE_CWD_FILE) == package_dir, "source target cwd is not its package directory")
  note("server-built target=" .. target_path .. " cwd=" .. package_dir)
  assert(stopped.session == session, "breakpoint belongs to a different session")
  local frames = request(session, "stackTrace", { threadId = stopped.body.threadId or 0, startFrame = 0, levels = 20 })
  local frame = assert(frames.stackFrames[1], "no real stack frame")
  assert(frame.name == "main.main", "unexpected frame: " .. vim.inspect(frame))
  assert(frame.source.path == source and frame.line == breakpoint_line, "source breakpoint location mismatch")
  local scopes = request(session, "scopes", { frameId = frame.id })
  local scope = assert(scopes.scopes[1], "no locals scope")
  assert(scope.variablesReference > 0, "locals reference missing")
  local variables = request(session, "variables", { variablesReference = scope.variablesReference })
  local known
  for _, variable in ipairs(variables.variables) do
    if variable.name == "known" then
      known = variable
    end
  end
  assert(known and known.value == "42" and known.type == "int", "real local known:int=42 missing: " .. vim.inspect(variables))
  note("target pid=" .. target_pid .. " main.main:" .. frame.line .. " known:int=42")

  local observer
  if retain_observer then
    -- Current nvim-dap closes the initiating client on its terminate response.
    -- A second real client cannot take that shortcut: only the server's
    -- terminated event may close it, exposing a missing terminal lifecycle.
    bingo.join(announcement)
    wait_for("second real nvim-dap observer joined the same live target", function()
      observer = dap.session()
      return observer ~= nil and observer ~= session and announced[observer] == announcement and attached[observer]
    end)
    assert(session_count() == 2 and counts.announcements == 2 and counts.user_events == 2,
      "observer was not registered exactly once")
    local joined = assert(http("/api/sessions"))
    assert(#joined == 1 and joined[1].id == announcement and joined[1].clients == 2
      and joined[1].state == "suspended", "observer disturbed the shared target")
    dap.set_session(session)
    note("retained observer: real nvim-dap sessions=2 managed sessions=1 clients=2")
  end

  assert(session.capabilities.supportsTerminateRequest, "server did not advertise terminate")
  counts.terminate_intents = counts.terminate_intents + 1
  dap.terminate({ on_done = guarded(function()
    counts.terminate_callbacks = counts.terminate_callbacks + 1
  end) })
  wait_for("single terminate response and actual nvim-dap completion callback", function()
    return counts.terminate_responses == 1 and counts.terminate_callbacks == 1
  end, 7000)
  wait_for("terminated native target is gone", function()
    return not alive(target_pid)
  end, 5000)
  wait_for("companion ID cleared and real nvim-dap sessions() empty", function()
    return bingo.session_id() == nil and dap.session() == nil and session_count() == 0
  end, 3000)
  assert(counts.close_callbacks == (retain_observer and 2 or 1), "Session.on_close did not run exactly once per client")
  if observer then
    assert(terminated[observer] == 1, "retained observer did not receive exactly one server terminated event")
  end
  wait_for("management session registry empty before idle shutdown", function()
    local remaining = http("/api/sessions")
    return remaining ~= nil and #remaining == 0
  end, 3000)
  wait_for("server-owned source build directory removed after session cleanup", function()
    return uv.fs_stat(vim.fs.dirname(target_path)) == nil
  end, 3000)
  wait_for("server-owned idle exit (no test signal)", function()
    return server_exit ~= nil
  end, idle_ms + 4000)
  assert(server_exit.code == 0 and server_exit.signal == 0, "server did not exit cleanly: " .. vim.inspect(server_exit))
  assert(not alive(server_pid), "server pid retained after exit callback")
  assert(text(server_log):find('msg="managed server idle timeout elapsed"', 1, true), "missing server idle evidence")
  assert(counts.terminate_intents == 1 and counts.terminate_responses == 1 and counts.terminate_callbacks == 1,
    "termination required more than one intent")
  bingo.dispose()
  check_errors()
  note("PASS " .. vim.json.encode(counts))
end

local function cleanup()
  target_pid = target_pid or tonumber(text(vim.env.BINGO_NVIM_SMOKE_PID_FILE))
  if target_pid == nil and server_pid ~= nil then
    -- A source launch can stall before main writes the PID file. The server's
    -- private TMPDIR and exact parent PID exclude another session's children.
    local result = vim.system({ "ps", "-ax", "-o", "pid=,ppid=" }, { text = true }):wait(1000)
    for line in (result.stdout or ""):gmatch("[^\n]+") do
      local pid, parent = line:match("^%s*(%d+)%s+(%d+)%s*$")
      local path = tonumber(parent) == server_pid and executable(pid) or nil
      if owned_target(path) then
        target_pid = tonumber(pid)
        target_path = path
      end
    end
  end
  if dap then
    for _, session in pairs(dap.sessions()) do
      pcall(session.close, session)
    end
  end
  if bingo then
    pcall(bingo.dispose)
  end
  if server_handle and not server_exit and not server_handle:is_closing() then
    server_handle:kill("sigterm")
    vim.wait(3000, function()
      return server_exit ~= nil
    end, 20)
  end
  if alive(target_pid) then
    local path = executable(target_pid)
    if owned_target(path) and (target_path == nil or path == target_path) then
      uv.kill(target_pid, 9)
    end
  end
  if server_handle and not server_exit and not server_handle:is_closing() then
    server_handle:kill("sigkill")
    vim.wait(3000, function()
      return server_exit ~= nil
    end, 20)
  end
  vim.wait(2000, function()
    return not alive(target_pid)
  end, 20)
  assert(not alive(target_pid), "cleanup could not retire exact target PID " .. tostring(target_pid))
  assert(server_handle == nil or server_exit ~= nil, "cleanup could not retire exact server PID " .. tostring(server_pid))
  note("failure cleanup: owned server and target gone")
end

local ok, failure = xpcall(smoke, debug.traceback)
if not ok then
  note("FAIL: " .. tostring(failure))
  note("counts before failure: " .. vim.json.encode(counts))
  note("server log: " .. server_log)
  if dap and management_port then
    local diagnostics_ok, diagnostics = pcall(function()
      return {
        dap_sessions = session_count(),
        companion_id = bingo and bingo.session_id(),
        target_alive = alive(target_pid),
        managed_sessions = http("/api/sessions"),
      }
    end)
    if diagnostics_ok then
      note("before failure cleanup: " .. vim.inspect(diagnostics))
    end
  end
  local cleanup_ok, cleanup_error = pcall(cleanup)
  if not cleanup_ok then
    note("cleanup failed: " .. tostring(cleanup_error))
  end
  vim.fn.writefile(vim.split(table.concat(observations, "\n"), "\n", { plain = true }), work .. "/result.log")
  vim.cmd("cquit 1")
else
  vim.fn.writefile(vim.split(table.concat(observations, "\n"), "\n", { plain = true }), work .. "/result.log")
  vim.fn.writefile({ "PASS" }, work .. "/success")
  vim.cmd("qa!")
end
