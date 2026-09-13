return function(test, equal, T)
  local config = require("bingo.config")
  local health = require("bingo.health")
  local session = require("bingo.session")
  local endpoint = { host = "127.0.0.1", port = 4711 }

  test("fixture isolation restores globals/modules and all cleanup after a failure", function()
    local real_vim, real_path, original_module = vim, package.path, package.loaded["bingo.config"]
    local cleaned = 0
    local restore = T.isolate()
    T.defer(function() cleaned = cleaned + 1 end)
    T.defer(function() error("injected cleanup failure") end)
    T.defer(function() cleaned = cleaned + 1 end)
    _G.vim, package.path = {}, "isolated"
    package.loaded["bingo.config"], package.preload["bingo_test_only"] = {}, function() end
    local ok, err = pcall(restore)
    equal(ok, false)
    T.contains(err, "injected cleanup failure")
    equal(cleaned, 2)
    equal(_G.vim, real_vim)
    equal(package.path, real_path)
    equal(package.loaded["bingo.config"], original_module)
    equal(package.preload["bingo_test_only"], nil)
  end)

  test("Go health and DAP constants drive the consumer compatibility fixture", function()
    local fixture = T.health()
    equal(health.validate(200, fixture, endpoint).kind, "compatible")
    equal(health.service, fixture.service)
    equal(health.management_api_version, fixture.managementApiVersion)
    equal(health.wire_protocol_version, fixture.wireProtocolVersion)
    equal(health.session_event_version, fixture.dap.sessionEventVersion)
    equal(session.event_version, fixture.dap.sessionEventVersion)
    equal(session.event_name, assert(T.read(T.root .. "/../../pkg/protocol/dap.go")
      :match('DAPSessionEventName%s*=%s*"([^"]+)"')))
    local announcement = assert(session.decode({
      version = fixture.dap.sessionEventVersion, sessionId = fixture.instanceId,
    }))
    equal(announcement.session_id, fixture.instanceId)
  end)

  test("managed defaults are drift checked against the shipped VS Code source", function()
    local text = T.read(T.root .. "/../vscode/src/configuration.ts")
    local options = config.normalize().server
    for key, name in pairs({
      management_host = "defaultManagementHost", dap_host = "defaultDapHost",
      mode = "defaultServerMode", management_port = "defaultManagementPort",
      dap_port = "defaultDapPort", ready_timeout_ms = "defaultServerReadyTimeoutMs",
      idle_timeout_ms = "defaultManagedIdleTimeoutMs",
    }) do
      local value = assert(text:match("export const " .. name .. " = ([^;]+);"))
      equal(options[key], tonumber(value) or value:match('^"(.-)"$'), key)
    end
  end)

  test("read-only native CI and the recipe invoke this dependency-free suite", function()
    local workflow = T.read(T.root .. "/../../.github/workflows/neovim-extension.yml")
    local recipe = T.read(T.root .. "/../../justfile")
    local command = "nvim --headless -u NONE -i NONE -l ./editors/neovim/tests/run.lua"
    T.contains(recipe:match("neovim%-check:%s*([^\n]+)"), command)
    T.contains(workflow, command)
    T.contains(workflow, "contents: read")
    T.contains(workflow, "ubuntu-24.04")
    T.contains(workflow, "macos-15")
    T.contains(workflow, "/download/v0.11.7/")
    T.contains(workflow, "--proto '=https' --proto-redir '=https'")
    T.contains(workflow, "shasum -a 256 -c -")
    T.contains(workflow, 'test "$(uname -m)" = "$EXPECTED_ARCH"')
    T.contains(workflow, "run: bash editors/neovim/scripts/integration.sh")
    T.contains(workflow, 'BINGO_NVIM_SMOKE_RETAIN_OBSERVER: "1"')
    T.contains(T.read(T.root .. "/scripts/integration.sh"),
      "--proto '=https' --proto-redir '=https'")
    T.contains(T.read(T.root .. "/tests/integration.lua"),
      'vim.env.BINGO_NVIM_SMOKE_RETAIN_OBSERVER ~= "0"')
    equal(workflow:find("pull_request_target", 1, true), nil)
    equal(workflow:find("contents: write", 1, true), nil)
    equal(workflow:find("linux-arm64", 1, true), nil)
    equal(workflow:find("macos-x86_64", 1, true), nil)
  end)

  test("normalization copies defaults without mutating caller tables", function()
    local input = { server = { dap_port = 14711 } }
    local options = config.normalize(input)
    options.server.dap_port = 2222
    equal(input.server.dap_port, 14711)
    equal(config.normalize().server.dap_port, 4711)
    equal(config.resolve({ dapPort = 12 }, config.normalize(input)).dap.port, 12)
  end)

  for _, value in ipairs({ false, 0, "", "auto" }) do
    test("non-table setup/server rejected: " .. tostring(value), function()
      T.raises(function() config.normalize(value) end, "setup options")
      T.raises(function() config.normalize({ server = value }) end, "server options")
    end)
  end
  for _, field in ipairs({ "configurations", "notify_session" }) do
    test(field .. " requires a boolean but preserves false", function()
      equal(config.normalize({ [field] = false })[field], false)
      T.raises(function() config.normalize({ [field] = 0 }) end, field)
    end)
  end
  test("log callback and path validation preserve literal argv strings", function()
    local log = function() end
    equal(config.normalize({ log = log }).log, log)
    T.raises(function() config.normalize({ log = "debug" }) end, "log")
    for _, field in ipairs({ "binary", "log_path" }) do
      local literal = "/tmp/directory with spaces/$(not a shell);bingo"
      equal(config.normalize({ server = { [field] = literal } }).server[field], literal)
      for _, bad in ipairs({ false, "", " \t", "a\0b" }) do
        T.raises(function() config.normalize({ server = { [field] = bad } }) end, field)
      end
    end
  end)

  for _, entry in ipairs({
    { "management_port", "managementPort", 1, 65535 },
    { "dap_port", "dapPort", 1, 65535 },
    { "ready_timeout_ms", "serverReadyTimeoutMs", 100, 120000 },
    { "idle_timeout_ms", "managedIdleTimeoutMs", 1, 86400000 },
  }) do
    local snake, camel, low, high = unpack(entry)
    for _, bad in ipairs({ low - 1, high + 1, 1.5, math.huge, -math.huge, 0 / 0, "123", false }) do
      test(camel .. " rejects invalid numeric value " .. tostring(bad), function()
        T.raises(function() config.normalize({ server = { [snake] = bad } }) end, snake)
        T.raises(function() config.resolve({ [camel] = bad }) end, camel)
      end)
    end
    test(camel .. " accepts both inclusive boundaries", function()
      for _, value in ipairs({ low, high }) do
        equal(config.normalize({ server = { [snake] = value } }).server[snake], value)
        config.resolve({ [camel] = value })
      end
    end)
  end
  for _, bad in ipairs({ "", "AUTO", "connectonly", false, 1 }) do
    test("unknown server mode rejected: " .. tostring(bad), function()
      T.raises(function() config.normalize({ server = { mode = bad } }) end, "server mode")
      T.raises(function() config.resolve({ serverMode = bad }) end, "server mode")
    end)
  end
  for _, entry in ipairs({
    { " DEBUG.internal ", "debug.internal" }, { "127.0.0.1", "127.0.0.1" },
    { "[::1]", "::1" }, { "::1", "::1" }, { "2001:db8::1", "2001:db8::1" },
    { "::ffff:127.0.0.1", "::ffff:127.0.0.1" }, { "fe80::1%en0", "fe80::1%en0" },
    { "[FE80::AB%TestNIC]", "fe80::ab%TestNIC" },
    { "debug-server.internal.", "debug-server.internal." },
  }) do
    test("host normalization: " .. entry[1], function()
      local options = config.normalize({ server = { dap_host = entry[1] } })
      equal(options.server.dap_host, entry[2])
      equal(config.resolve({ dapHost = entry[1], serverMode = "connectOnly" }).dap.host, entry[2])
    end)
  end
  for _, bad in ipairs({
    "", " ", "a\r\nInjected: yes", "a\0b", "a b", "http://host", "host/path",
    "host:4711", "host?query", "host#fragment", "[broken", ":::1", "[dns.internal]", "[127.0.0.1]",
    "1:2:3:4:5:6:7", "1:2:3:4:5:6:7:8:9", "::ffff:999.1.1.1", "::g", "a%zone",
  }) do
    test("host rejects delimiters or malformed IPv6: " .. string.format("%q", bad), function()
      for _, field in ipairs({ "managementHost", "dapHost" }) do
        T.raises(function() config.resolve({ [field] = bad }) end, field)
      end
    end)
  end
  test("IPv6 endpoint rendering brackets only the authority", function()
    equal(config.endpoint({ host = "::1", port = 6060 }), "[::1]:6060")
  end)
  test("auto rejects colliding endpoints while connectOnly remains permissive", function()
    T.raises(function() config.resolve({ dapPort = 6060 }) end, "distinct")
    equal(config.resolve({ dapPort = 6060, serverMode = "connectOnly" }).dap.port, 6060)
  end)

  local valid_requests = {
    { request = "launch", program = "/tmp/a b;$(x)", args = { "", "a b" }, env = { "X=a=b" } },
    { request = "attach", pid = 1, binaryPath = "", stopOnEntry = false },
    { request = "attach", pid = 2147483647, binaryPath = "/tmp/target" },
    { request = "attach", session = "session-1._" },
  }
  for index, value in ipairs(valid_requests) do
    test("valid launch/attach/join request " .. index, function()
      config.validate_request(value)
    end)
  end
  for _, entry in ipairs({
    { {}, "request" }, { false, "configuration" },
    { { request = "launch" }, "program" },
    { { request = "launch", program = " " }, "program" },
    { { request = "launch", program = "/x", pid = 1 }, "launch cannot" },
    { { request = "launch", program = "/x", session = "s" }, "launch cannot" },
    { { request = "launch", program = "/x", args = { [2] = "x" } }, "args" },
    { { request = "launch", program = "/x", env = { X = "x" } }, "env" },
    { { request = "launch", program = "/x", args = { 1 } }, "args" },
    { { request = "launch", program = "/x", args = { "a\0b" } }, "args" },
    { { request = "launch", program = "/x", stopOnEntry = 1 }, "stopOnEntry" },
    { { request = "attach" }, "exactly one" },
    { { request = "attach", pid = 1, session = "s" }, "exactly one" },
    { { request = "attach", pid = 1, program = "/x" }, "attach cannot" },
    { { request = "attach", pid = "1" }, "pid" },
    { { request = "attach", pid = 0 }, "pid" },
    { { request = "attach", pid = 1.5 }, "pid" },
    { { request = "attach", pid = math.huge }, "pid" },
    { { request = "attach", pid = 0 / 0 }, "pid" },
    { { request = "attach", pid = 1, binaryPath = false }, "binaryPath" },
    { { request = "attach", session = "../session" }, "sessionId" },
    { { request = "attach", session = "" }, "sessionId" },
  }) do
    test("request validation rejects " .. vim.inspect(entry[1]), function()
      T.raises(function() config.validate_request(entry[1]) end, entry[2])
    end)
  end

  for _, entry in ipairs({
    { "service", "foreign", "identity" }, { "service", false, "identity" },
    { "managementApiVersion", "1", "management API" },
    { "managementApiVersion", 2, "management API" },
    { "wireProtocolVersion", "1.2", "wire protocol" },
    { "wireProtocolVersion", "", "wire protocol" },
    { "instanceId", false, "instanceId" }, { "instanceId", " ", "instanceId" },
    { "dap", false, "not enabled" },
  }) do
    test("health rejects incompatible field " .. entry[1] .. "=" .. tostring(entry[2]), function()
      local fixture = T.health()
      fixture[entry[1]] = entry[2]
      local result = health.validate(200, fixture, endpoint)
      equal(result.kind, "incompatible")
      T.contains(result.reason, entry[3])
    end)
  end
  for _, field in ipairs({ "service", "managementApiVersion", "wireProtocolVersion", "instanceId", "dap" }) do
    test("health requires " .. field, function()
      local fixture = T.health()
      fixture[field] = nil
      equal(health.validate(200, fixture, endpoint).kind, "incompatible")
    end)
  end
  for _, entry in ipairs({
    { "enabled", false, "not enabled" }, { "enabled", 1, "not enabled" },
    { "sessionEventVersion", 0, "session event" },
    { "sessionEventVersion", "1", "session event" },
    { "address", "127.0.0.1:14711", "port" },
    { "address", "foreign:4711", "host" },
    { "address", "127.0.0.1:0", "invalid" },
    { "address", "127.0.0.1:65536", "invalid" },
    { "address", "::1:4711", "invalid" },
    { "address", false, "invalid" },
  }) do
    test("health rejects DAP " .. entry[1] .. "=" .. tostring(entry[2]), function()
      local fixture = T.health()
      fixture.dap[entry[1]] = entry[2]
      local result = health.validate(200, fixture, endpoint)
      equal(result.kind, "incompatible")
      T.contains(result.reason, entry[3])
    end)
  end
  for _, address in ipairs({ "0.0.0.0:4711", "[::]:4711" }) do
    test("wildcard health retains configured connect host: " .. address, function()
      local fixture = T.health()
      fixture.dap.address = address
      equal(health.validate(200, fixture, endpoint).kind, "compatible")
      equal(endpoint.host, "127.0.0.1")
    end)
  end
  test("health accepts forward-compatible fields but not a non-object", function()
    local fixture = T.health()
    fixture.future = { enabled = true }
    equal(health.validate(200, fixture, endpoint).kind, "compatible")
    equal(health.validate(200, false, endpoint).kind, "incompatible")
    T.contains(health.validate(503, fixture, endpoint).reason, "HTTP 503")
  end)
  for _, body in ipairs({
    false, "", {}, { version = 1 }, { sessionId = "a" },
    { version = "1", sessionId = "a" }, { version = 2, sessionId = "a" },
    { version = 1, sessionId = "a", extra = true }, { 1, "a" },
    { version = 1, sessionId = false }, { version = 1, sessionId = string.rep("a", 129) },
    { version = 1, sessionId = "\na" }, { version = 1, sessionId = "a/b" },
  }) do
    test("session body rejects " .. vim.inspect(body), function()
      local value, message = session.decode(body)
      equal(value, nil)
      assert(type(message) == "string" and #message > 0)
    end)
  end
  test("session identifiers accept exactly the 128-byte boundary", function()
    equal(assert(session.decode({ version = 1, sessionId = string.rep("a", 128) })).session_id,
      string.rep("a", 128))
  end)
end
