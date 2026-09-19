return function(test, equal, T)
  local config = require("bingo.config")
  local function in_directory(path)
    local real = vim
    _G.vim = setmetatable({
      fn = setmetatable({ getcwd = function() return path end }, { __index = real.fn }),
    }, { __index = real })
  end

  test("a real Go buffer in a space-containing path selects its source package", function()
    local directory = assert(vim.uv.fs_realpath(T.tempdir()))
    local source = directory .. "/main.go"
    assert(vim.fn.writefile({ "package main", "func main() {}" }, source) == 0)
    local previous = vim.api.nvim_get_current_buf()
    local scratch = vim.api.nvim_create_buf(false, true)
    local opened
    T.defer(function()
      vim.api.nvim_set_current_buf(previous)
      if opened and opened ~= scratch then vim.api.nvim_buf_delete(opened, { force = true }) end
      vim.api.nvim_buf_delete(scratch, { force = true })
    end)
    vim.api.nvim_set_current_buf(scratch)
    vim.cmd.edit(vim.fn.fnameescape(source))
    opened = vim.api.nvim_get_current_buf()
    equal(vim.api.nvim_buf_get_name(0), source)
    equal(config.package_directory(), directory)
  end)

  test("local source preparation resolves program against cwd and leaves user config unchanged", function()
    local root = T.tempdir()
    local cwd = root .. "/work tree"
    local package_dir = cwd .. "/cmd/tool"
    assert(vim.fn.mkdir(package_dir, "p") == 1)
    in_directory(root)
    local input = {
      request = "launch", mode = "debug", program = "cmd/tool", cwd = "work tree",
      args = { "literal;$(not a shell)", "" }, env = { "X=a=b" },
    }
    local prepared = config.prepare_request(input)
    equal(prepared.program, package_dir)
    equal(prepared.cwd, cwd)
    equal(prepared.args, input.args)
    equal(prepared.env, input.env)
    equal(input.program, "cmd/tool")
    equal(input.cwd, "work tree")
  end)
  test("local source default cwd is the resolved package rather than a shared server cwd", function()
    local root = T.tempdir()
    local package_dir = root .. "/cmd"
    assert(vim.fn.mkdir(package_dir) == 1)
    in_directory(root)
    local prepared = config.prepare_request({ request = "launch", mode = "debug", program = "./cmd" })
    equal(prepared.program, package_dir)
    equal(prepared.cwd, package_dir)
  end)
  test("local source paths retain spaces, dollar signs and shell metacharacters literally", function()
    local root = T.tempdir()
    local package_dir = root .. "/hello $HOME;[test] 'app'"
    assert(vim.fn.mkdir(package_dir) == 1)
    local prepared = config.prepare_request({ request = "launch", mode = "debug", program = package_dir })
    equal(prepared.program, package_dir)
    equal(prepared.cwd, package_dir)
  end)
  test("source launch validates both package and cwd as directories before transport", function()
    local root = T.tempdir()
    local source = root .. "/main.go"
    assert(vim.fn.writefile({ "package main" }, source) == 0)
    for _, bad in ipairs({ root .. "/missing", source }) do
      T.raises(function()
        config.prepare_request({ request = "launch", mode = "debug", program = bad })
      end, "program")
      for _, mode in ipairs({ "debug", "exec" }) do
        T.raises(function()
          config.prepare_request({ request = "launch", mode = mode, program = root, cwd = bad })
        end, "cwd")
      end
    end
  end)
  test("legacy binary launches keep omitted mode and cwd with server-relative program", function()
    local input = { request = "launch", program = "bin/target" }
    local prepared = config.prepare_request(input)
    equal(prepared.mode, nil)
    equal(prepared.cwd, nil)
    equal(prepared.program, "bin/target")
    local root = T.tempdir()
    in_directory(root)
    prepared = config.prepare_request({ request = "launch", mode = "exec", program = "bin/target", cwd = "." })
    equal(prepared.mode, "exec")
    equal(prepared.cwd, root)
    equal(prepared.program, "bin/target")
  end)
  test("connectOnly source paths remain entirely server-local without filesystem access", function()
    local real = vim
    _G.vim = setmetatable({
      uv = setmetatable({ fs_stat = function() error("remote paths inspected locally") end }, { __index = real.uv }),
    }, { __index = real })
    local input = { request = "launch", mode = "debug", program = "../remote package", cwd = "/server only" }
    local prepared = config.prepare_request(input, config.normalize({ server = { mode = "connectOnly" } }))
    equal(prepared.program, input.program)
    equal(prepared.cwd, input.cwd)
    input.serverMode = "connectOnly"
    input.cwd = nil
    prepared = config.prepare_request(input)
    equal(prepared.cwd, nil)
    equal(prepared.program, input.program)
  end)
  test("request preparation rejects malformed lifecycle overrides before dap.run", function()
    for _, override in ipairs({ { serverMode = "debug" }, { dapPort = 6060 }, { dapPort = 0 } }) do
      local input = vim.tbl_extend("force", { request = "launch", mode = "debug", program = "." }, override)
      local ok = pcall(config.prepare_request, input)
      equal(ok, false)
    end
  end)
end
