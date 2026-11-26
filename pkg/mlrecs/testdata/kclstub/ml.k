# A stand-in for forge's kcl/ml.k, written from the KCL contract the ML
# slice settled (ml/kcl-mlstack implements the real one). TestProposalIsValidKCL
# runs every proposal against it, so a syntax error, a misspelt field or a
# broken invariant in what Propose writes fails here, before the real schema
# lands. When kcl/ml.k is on main, the CLI's e2e renders proposals against it.
import regex

schema ClusterTarget:
    cluster: str
    namespace: str
    connected_cluster?: str

schema HelmChart:
    name: str
    chart: str = ""
    repo: str = ""
    oci: str = ""
    version: str
    namespace: str
    values: {str:any} = {}
    delivery: "bootstrap" | "bundle" = "bootstrap"

    check:
        (repo == "") != (oci == ""), "HelmChart '${name}': set exactly one of repo and oci"
        oci != "" or chart != "", "HelmChart '${name}': a repo chart names its chart"
        version != "", "HelmChart '${name}': version is required"

_ML_ADOPT_ONLY = ["kuberay"]

schema Adopt:
    kind: str
    namespace?: str
    selector?: {str:str}
    port?: int
    allow: [str] = []
    version?: str
    note?: str

    check:
        regex.match(kind, r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"), "Adopt: kind '${kind}' is not RFC-1123"
        port == None or selector != None, "Adopt '${kind}': port needs selector"
        not allow or port != None, "Adopt '${kind}': allow needs port"

schema Install:
    kind: str
    chart: HelmChart
    reason?: str
    selector?: {str:str}
    port?: int
    allow: [str] = []

    check:
        kind not in _ML_ADOPT_ONLY or reason, "Install '${kind}': adopt-only by policy"
        port == None or selector != None, "Install '${kind}': port needs selector"
        not allow or port != None, "Install '${kind}': allow needs port"

type MLComponent = Adopt | Install

schema Accelerators:
    vendor: "nvidia" | "amd" | "tpu" | "neuron" | "gaudi" | "generic"
    resource?: str
    metrics?: MLComponent
    node_selector?: {str:str}

    check:
        vendor != "generic" or resource, "Accelerators: a generic vendor names its resource"

schema RecommendationDecision:
    id: str
    decision: "dismiss" = "dismiss"
    reason: str
    expires: str

    check:
        regex.match(id, r"^[a-z0-9]+(-[a-z0-9]+)*$"), "RecommendationDecision: bad id '${id}'"
        reason, "RecommendationDecision '${id}': reason is required"
        regex.match(expires, r"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"), "RecommendationDecision '${id}': expires is YYYY-MM-DD"

schema MLStack:
    cluster: ClusterTarget
    accelerators: [Accelerators] = []
    node_health?: MLComponent
    queue?: MLComponent
    training: [MLComponent] = []
    serving: [MLComponent] = []
    monitoring: [MLComponent] = []
    pull_cache?: MLComponent
    observed: [str] = []
    observed_at?: str
    decisions: [RecommendationDecision] = []

    _all = [c for c in [node_health, queue, pull_cache] if c != None] + training + serving + monitoring + [a.metrics for a in accelerators if a.metrics != None]
    _installed = [c.kind for c in _all if typeof(c) == "Install"]
    _adopted = [c.kind for c in _all if typeof(c) == "Adopt"]

    check:
        all k in _installed {k not in _adopted}, "a kind is both adopted and installed"
        len(_installed) == len({k: None for k in _installed}), "a kind is installed twice"
        all k in _installed {k not in observed}, "discovery found a kind running (observed); adopt it instead of installing a second one"
        len([a.vendor for a in accelerators]) == len({a.vendor: None for a in accelerators}), "accelerator vendors repeat"
        len([d.id for d in decisions]) == len({d.id: None for d in decisions}), "decision ids repeat"
