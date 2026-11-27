"""
Positive test — Frontend.markdown_negotiation and the edge-log opt-out.

Pins:
  1. A hosted frontend with markdown_negotiation renders it; one without
     renders no key (opt-in, byte-identical to before the field).
  2. `ObservabilitySignals {edge = False}` renders `edge_logs: false` on each
     hosted frontend, and `Observability {enabled = False}` does too.
  3. An env that keeps edge logs (the default) renders no `edge_logs` key.
  4. `ObservabilityRetention.edge_days` renders only when set.
"""

import forge

_fe = lambda md: bool -> forge.Frontend {
    forge.Frontend {
        name = "web"
        image = "ghcr.io/acme/web"
        path = "web"
        type = "vite"
        markdown_negotiation = md
        runtime = forge.OnHosted {}
    }
}

_render = lambda md: bool, obs: any -> any {
    forge.render(forge.Bundle {
        project = "fixture"
        control_plane = forge.ControlPlane {}
        frontends = [_fe(md)]
        observability = obs
    })
}

_plain = _render(False, forge.Observability {})
_md = _render(True, forge.Observability {})
_edge_off = _render(False, forge.Observability {signals = forge.ObservabilitySignals {edge = False}})
_obs_off = _render(False, forge.Observability {enabled = False})
_days = _render(False, forge.Observability {retention = forge.ObservabilityRetention {edge_days = 7}})

assert_md_on = _md.frontends[0].markdown_negotiation == True
assert_md_absent = "markdown_negotiation" not in _plain.frontends[0]
assert_edge_absent = "edge_logs" not in _plain.frontends[0]
assert_edge_signal_off = _edge_off.frontends[0].edge_logs == False
assert_edge_obs_off = _obs_off.frontends[0].edge_logs == False
assert_edge_days = _days.observability.retention.edge_days == 7
assert_edge_days_absent = "edge_days" not in _plain.observability.retention
