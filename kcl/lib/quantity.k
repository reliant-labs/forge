"""
Neutral compute-quantity FORMATTERS — one per deploy target.

`forge.Resources` holds TARGET-NEUTRAL quantities: CPU in millicores (int),
memory in bytes (int). Neither is k8s- nor compose-dialect, so no target
vocabulary leaks into the portable core (the same principle as env-as-map:
the core names the neutral fact, each ADAPTER formats it for its target).

These lambdas are the per-target formatters every adapter call site shares
(render.k `_render_resources`, lib/services.k + components/expand.k
`_resource_block` for k8s; core.k `_service_to_compose` for compose):

  * k8s     — millicores -> "<n>m", or a bare whole-core "<n>" when evenly
              divisible by 1000; bytes -> binary "<n>Gi"/"<n>Mi" when a clean
              multiple, else the raw byte count (k8s accepts a bare-int
              quantity). Round-trips the historical strings: 100 -> "100m",
              1000 -> "1", 134217728 -> "128Mi".
  * compose — millicores -> DECIMAL cores string (250 -> "0.25", 1000 -> "1",
              1500 -> "1.5"); bytes -> the raw byte int, which is a valid
              compose `deploy.resources` memory value (unambiguous, no
              binary/decimal-suffix ambiguity).

Author-facing helpers that PRODUCE these neutral values (forge.cpu / mcpu /
mib / gib) live top-level in the forge package so `forge.cpu(0.25)` resolves;
these formatters are the adapter-facing inverse.
"""

_mi = 1024 * 1024
_gi = 1024 * 1024 * 1024

# ── k8s formatters ─────────────────────────────────────────────────────────
# millicores -> k8s cpu quantity. Whole cores render bare ("1"), otherwise
# the milli suffix ("250m") — reproducing the strings Resources used to hold.
fmt_k8s_cpu = lambda millicores: int -> str {
    str(millicores // 1000) if millicores % 1000 == 0 else str(millicores) + "m"
}

# bytes -> k8s memory quantity, preferring binary Gi/Mi; falls back to a
# bare-int byte count (a legal k8s quantity) when not a clean multiple.
fmt_k8s_mem = lambda b: int -> str {
    str(b // _gi) + "Gi" if b % _gi == 0 else \
        (str(b // _mi) + "Mi" if b % _mi == 0 else str(b))
}

# ── compose formatters ─────────────────────────────────────────────────────
# millicores -> decimal cores string. Whole cores render as an integer ("1");
# fractional cores keep only the significant decimal digits ("0.25", "1.5").
fmt_compose_cpu = lambda millicores: int -> str {
    _whole = millicores // 1000
    _frac = millicores % 1000
    str(_whole) if _frac == 0 else \
        str(_whole) + "." + str(1000 + _frac)[1:].rstrip("0")
}

# bytes -> compose memory (raw byte int — a valid compose deploy.resources
# value, and dialect-free unlike a "512M"/"512Mi" suffixed string).
fmt_compose_mem = lambda b: int -> int {
    b
}
