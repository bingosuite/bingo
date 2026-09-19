if vim.g.loaded_bingo == 1 then
  return
end
vim.g.loaded_bingo = 1

local function path_argument(value)
  if value == nil or value == "" then
    return nil
  end
  -- fargs only partially decodes multiargument paths and leaves single-argument
  -- paths escaped. Decode completion escaping exactly once from the raw text.
  return (value:gsub("\\(.)", "%1"))
end

vim.api.nvim_create_user_command("BingoDebug", function(command)
  require("bingo").debug(path_argument(command.args))
end, {
  nargs = "?",
  complete = "dir",
  desc = "Build and debug a Go package with bingo through nvim-dap",
})

vim.api.nvim_create_user_command("BingoLaunch", function(command)
  require("bingo").launch(path_argument(command.args))
end, {
  nargs = "?",
  complete = "file",
  desc = "Launch a binary with bingo through nvim-dap",
})

vim.api.nvim_create_user_command("BingoAttach", function(command)
  local pid_text, binary_path = command.args:match("^%s*(%S+)%s*(.*)$")
  local pid = tonumber(pid_text)
  if pid_text ~= nil and pid == nil then
    vim.notify("BingoAttach requires a positive PID and optional binary path",
      vim.log.levels.ERROR, { title = "bingo" })
    return
  end
  require("bingo").attach(pid, path_argument(binary_path))
end, {
  nargs = "*",
  desc = "Attach bingo to an operating-system process through nvim-dap",
})

vim.api.nvim_create_user_command("BingoJoin", function(command)
  require("bingo").join(command.args ~= "" and command.args or nil)
end, {
  nargs = "?",
  desc = "Join an existing managed bingo session through nvim-dap",
})

vim.api.nvim_create_user_command("BingoSession", function()
  require("bingo").show_session()
end, {
  desc = "Show the active managed bingo session ID",
})
