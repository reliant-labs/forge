package generator

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func transportFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := writeHooksPackage(root, FrontendWorkspaceLayout{HooksPackage: "@fixture/hooks", APIPackage: "@fixture/api"}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "packages", "hooks")
}

func TestWorkspaceTransportSourceAndOwnership(t *testing.T) {
	dir := transportFixture(t)
	path := filepath.Join(dir, "src", "transport.ts")
	source := mustRead(t, path)
	for _, want := range []string{"const forwardingTransport: Transport", "getApiTransport().unary(...args)", "getApiTransport().stream(...args)", "createClient(service, forwardingTransport)"} {
		if !strings.Contains(source, want) {
			t.Errorf("transport missing %q", want)
		}
	}
	for _, file := range []string{"use-api-query.ts", "use-api-mutation.ts"} {
		if !strings.Contains(mustRead(t, filepath.Join(dir, "src", file)), `from "@reliantlabs/forge-web-runtime/interceptors"`) {
			t.Errorf("%s must use portable error import", file)
		}
	}
	// Even the old broken scaffold is owned: upgrading must not erase edits.
	owned := "// custom transport/interceptors\n" + strings.Replace(source, "createClient(service, forwardingTransport)", "createClient(service, getApiTransport())", 1)
	if err := os.WriteFile(path, []byte(owned), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(dir))
	if err := writeHooksPackage(root, FrontendWorkspaceLayout{HooksPackage: "@fixture/hooks", APIPackage: "@fixture/api"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, path); got != owned {
		t.Fatal("regeneration overwrote owned transport")
	}
}

// This fixture compiles the emitted transport and root barrel with real Connect
// and protobuf, then executes the service module's top-level client. No browser,
// React renderer, server, or fake implementation of createClient is involved.
func TestWorkspaceTransportModuleBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("installs TypeScript/Connect and runs tsc and node; runs in full mode")
	}
	for _, tool := range []string{"node", "npm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: executable transport behavior not verified", tool)
		}
	}
	dir := transportFixture(t)
	put := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Replace only the fixture's workspace manifest, not production output. Pin
	// test dependencies so the executable regression isn't a moving latest test.
	put("package.json", `{"private":true,"dependencies":{"@bufbuild/protobuf":"2.5.0","@connectrpc/connect":"2.0.0","@tanstack/react-query":"5.62.0","react":"18.3.1"},"devDependencies":{"typescript":"5.8.3","@types/react":"18.3.18"}}`)
	put("tsconfig.json", `{"compilerOptions":{"target":"ES2022","module":"CommonJS","moduleResolution":"node","strict":true,"skipLibCheck":true,"outDir":"dist"},"include":["src/**/*.ts"]}`)
	// The wrapper's error import is type-only and not under test here.
	put("src/runtime.d.ts", `declare module "@reliantlabs/forge-web-runtime/interceptors" { export type ConnectClientError = Error; }
declare module "@reliantlabs/forge-web-runtime" { export type ConnectClientError = Error; }`)
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("fixture.proto"), Package: proto.String("fixture"), Syntax: proto.String("proto3"), MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Message")}}, Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Fixture"), Method: []*descriptorpb.MethodDescriptorProto{
		{Name: proto.String("Read"), InputType: proto.String(".fixture.Message"), OutputType: proto.String(".fixture.Message")},
		{Name: proto.String("Mutate"), InputType: proto.String(".fixture.Message"), OutputType: proto.String(".fixture.Message")},
		{Name: proto.String("Watch"), InputType: proto.String(".fixture.Message"), OutputType: proto.String(".fixture.Message"), ServerStreaming: proto.Bool(true)},
	}}}}
	raw, err := proto.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	put("src/generated/index.ts", `import { fileDesc, serviceDesc } from "@bufbuild/protobuf/codegenv1";
import { connectClient } from "../transport";
export const service = serviceDesc(fileDesc("`+base64.StdEncoding.EncodeToString(raw)+`"), 0);
export const client = connectClient(service);
`)
	put("behavior.cjs", transportBehaviorJS)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(command string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", command, args, err, out)
		}
		t.Logf("%s: %s", command, out)
	}
	run("npm", "install", "--ignore-scripts", "--no-audit", "--no-fund")
	run("node", "node_modules/typescript/bin/tsc")
	run("node", "behavior.cjs")
}

const transportBehaviorJS = `const assert = require("node:assert/strict");
// Requiring only the setter from the actual emitted root barrel evaluates its
// generated service dependency before bootstrap can register a transport.
const api = require("./dist/index.js");
const { client, service, setApiTransport, getApiTransport } = api;
assert.equal(typeof api.useApiQuery, "function");
assert.equal(typeof api.useApiMutation, "function");
assert.equal(typeof api.connectClient, "function");
const unset = /ApiTransport not initialized.*setApiTransport/;
const collect = async (stream) => { const values = []; for await (const value of stream) values.push(value); return values; };
(async () => {
  assert.throws(() => getApiTransport(), unset);
  await assert.rejects(async () => client.read({}), unset);
  await assert.rejects(async () => collect(client.watch({})), unset);
  const calls = [];
  const failure = new Error("transport failure");
  const make = (name) => ({
    unary(...args) {
      assert.equal(this, transports[name]);
      calls.push({ name, kind: "unary", args });
      if (args[4].fail) return Promise.reject(failure);
      return Promise.resolve({ message: { name }, header: new Headers(), trailer: new Headers() });
    },
    stream(...args) {
      assert.equal(this, transports[name]);
      calls.push({ name, kind: "stream", args });
      return Promise.resolve({ message: (async function* () { yield { name }; yield { name }; })(), header: new Headers(), trailer: new Headers() });
    },
  });
  const transports = { a: make("a"), b: make("b") };
  setApiTransport(transports.a);
  assert.equal(getApiTransport(), transports.a);
  const signal = new AbortController().signal;
  const contextValues = require("@connectrpc/connect").createContextValues();
  const options = { signal, timeoutMs: 123, headers: { "x-fixture": "yes" }, contextValues };
  const input = { value: "request" };
  assert.deepEqual(await client.read(input, options), { name: "a" });
  assert.deepEqual(await client.mutate(input, options), { name: "a" });
  const active = client.watch(input, options)[Symbol.asyncIterator]();
  assert.deepEqual((await active.next()).value, { name: "a" });
  const pending = client.read(input, options);
  setApiTransport(transports.b);
  assert.deepEqual(await pending, { name: "a" });
  assert.deepEqual((await active.next()).value, { name: "a" });
  assert.equal((await active.next()).done, true);
  assert.equal(getApiTransport(), transports.b);
  assert.deepEqual(await client.read(input, options), { name: "b" });
  assert.deepEqual(await client.mutate(input, options), { name: "b" });
  assert.deepEqual(await collect(client.watch(input, options)), [{ name: "b" }, { name: "b" }]);
  for (const { args } of calls) {
    assert.equal(args[0].parent, service);
    assert.equal(args[1], signal);
    assert.equal(args[2], 123);
    assert.equal(new Headers(args[3]).get("x-fixture"), "yes");
    assert.equal(args[5], contextValues);
  }
  for (const { args } of calls.filter(c => c.kind === "unary")) assert.equal(args[4], input);
  for (const { args } of calls.filter(c => c.kind === "stream")) assert.deepEqual(await collect(args[4]), [input]);
  await assert.rejects(client.read({ fail: true }), err => err === failure);
  console.log("PASS: root import, unset calls, unary/mutation/stream forwarding, arguments, receiver, swap, in-flight calls, errors");
})().catch(err => { console.error(err); process.exitCode = 1; });
`
