return function(test, equal, T)
  local function fixture()
    local root = T.tempdir() .. "/repo with spaces;literal"
    assert(vim.fn.mkdir(root .. "/editors/neovim/scripts", "p") == 1)
    root = assert(vim.uv.fs_realpath(root))
    assert(vim.fn.mkdir(root .. "/scripts") == 1)
    assert(vim.fn.writefile({ "module github.com/bingosuite/bingo", "go 1.25.5" }, root .. "/go.mod") == 0)
    local script = root .. "/editors/neovim/scripts/prepare.sh"
    assert(vim.fn.writefile(vim.split(T.read(T.root .. "/scripts/prepare.sh"), "\n", { plain = true }), script) == 0)
    local function helper(lines)
      assert(vim.fn.writefile(lines, root .. "/scripts/build-binary.sh") == 0)
    end
    local function run(args)
      return vim.system(vim.list_extend({ "bash", script }, args or {}), {
        cwd = vim.fs.dirname(root),
        text = true,
        env = { BINGO_PREPARE_TEST_LOG = root .. "/invocation" },
      }):wait(5000)
    end
    return root, helper, run
  end

  test("prepare script delegates literal native output argv from any cwd without extra tools", function()
    local root, helper, run = fixture()
    helper({
      "#!/usr/bin/env bash",
      "set -euo pipefail",
      'printf "%s\\n" "$#" "$@" > "$BINGO_PREPARE_TEST_LOG"',
      'mkdir -p "$(dirname "$2")"',
      'printf "prepared\\n" > "$2"',
    })
    local result = run()
    equal(result.code, 0, result.stderr)
    equal(T.read(root .. "/invocation"), "2\nbingo\n" .. root .. "/editors/neovim/bin/bingo\n")
    equal(T.read(root .. "/editors/neovim/bin/bingo"), "prepared\n")
  end)
  test("prepare rejects extra arguments before invoking the native builder", function()
    local root, helper, run = fixture()
    helper({ 'echo invoked > "$BINGO_PREPARE_TEST_LOG"' })
    local result = run({ "linux", "amd64" })
    equal(result.code, 1)
    T.contains(result.stderr, "no arguments")
    equal(vim.fn.filereadable(root .. "/invocation"), 0)
  end)
  test("prepare explains a standalone plugin checkout without a shared build helper", function()
    local _, _, run = fixture()
    local result = run()
    equal(result.code, 1)
    T.contains(result.stderr, "full bingosuite/bingo checkout")
    T.contains(result.stderr, "bundled bin/bingo or set server.binary")
  end)
  test("prepare preserves builder failures and does not overwrite a working binary", function()
    local root, helper, run = fixture()
    helper({ 'echo "ERROR: native build prerequisite missing" >&2', "exit 28" })
    assert(vim.fn.mkdir(root .. "/editors/neovim/bin") == 1)
    assert(vim.fn.writefile({ "previous server" }, root .. "/editors/neovim/bin/bingo") == 0)
    local result = run()
    equal(result.code, 28)
    T.contains(result.stderr, "native build prerequisite missing")
    equal(T.read(root .. "/editors/neovim/bin/bingo"), "previous server\n")
  end)
end
