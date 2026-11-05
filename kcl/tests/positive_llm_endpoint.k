"""
Positive test — a forge.LLM on a forge.LLMEndpoint (dev: Reliant's public
gateway with your own token, or a local Ollama).

  1. a host process reads the root, root + /v1 for OpenAI, and the key as
     the forge.ManagedSecret it declares (the env's secret store);
  2. a hosted referrer of an endpoint is legal too, with a ManagedSecret key;
  3. a declared openai_base_url replaces the derived one;
  4. a compose container gets literals (a local endpoint's literal key).
"""

import forge
import forge.workloads as fw

_img = "ghcr.io/acme/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_gw = forge.LLMEndpoint {
    base_url = "https://admin.reliantapi.com"
    api_key = forge.ManagedSecret {name = "RELIANT_LLM_API_KEY"}
}

_host = forge.render(forge.Bundle {
    project = "acme"
    lifecycle = "local"
    llm = forge.LLM {runtime = _gw}
    workloads = [fw.Workload {
        name = "api"
        env = forge.llm_env()
        command = ["./bin/api"]
        runtime = forge.OnHost {}
    }]
})

_hosted = forge.render(forge.Bundle {
    project = "acme"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com"}
    llm = forge.LLM {runtime = forge.LLMEndpoint {
        base_url = "https://llm.example.com"
        openai_base_url = "https://llm.example.com/openai/v1"
        api_key = forge.ManagedSecret {name = "LLM_KEY"}
    }}
    workloads = [fw.Workload {
        name = "api"
        image = _img
        env = forge.llm_env()
        runtime = forge.OnHosted {}
    }]
})

_compose = forge.render(forge.Bundle {
    project = "acme"
    lifecycle = "local"
    llm = forge.LLM {runtime = forge.LLMEndpoint {base_url = "http://host.docker.internal:11434", api_key = "ollama"}}
    workloads = [fw.Workload {
        name = "indexer"
        kind = "worker"
        env = forge.llm_env()
        runtime = forge.OnCompose {}
    }]
})

_h = {e.name: e for e in _host.workloads[0].spec.env}
_d = {e.name: e for e in _hosted.workloads[0].spec.env}
_c = _compose.workloads[0].runtime.env

# 1. A host process.
assert_host_base = _h.RELIANT_LLM_BASE_URL.value == "https://admin.reliantapi.com" and _h.ANTHROPIC_BASE_URL.value == "https://admin.reliantapi.com"
assert_host_openai = _h.OPENAI_BASE_URL.value == "https://admin.reliantapi.com/v1"
assert_host_key = all k in ["RELIANT_LLM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"] { _h[k].managedSecret.name == "RELIANT_LLM_API_KEY" and not _h[k].value }

# 2 + 3. A hosted referrer of an endpoint, with a declared OpenAI base.
assert_hosted_openai = _d.OPENAI_BASE_URL.value == "https://llm.example.com/openai/v1"
assert_hosted_key = _d.OPENAI_API_KEY.managedSecret.name == "LLM_KEY"

# 4. Compose: literals.
assert_compose_literals = _c.OPENAI_BASE_URL == "http://host.docker.internal:11434/v1" and _c.OPENAI_API_KEY == "ollama"
