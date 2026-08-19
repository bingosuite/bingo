if vim.g.loaded_bingo == 1 then
  return
end
vim.g.loaded_bingo = 1

vim.api.nvim_create_user_command("BingoLaunch", function(command)
  require("bingo").launch(command.args ~= "" and command.args or nil)
end, {
  nargs = "?",
  complete = "file",
  desc = "Launch a binary with bingo through nvim-dap",
})

vim.api.nvim_create_user_command("BingoAttach", function(command)
  local pid = tonumber(command.fargs[1])
  require("bingo").attach(pid, command.fargs[2])
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
