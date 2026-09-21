"""
Positive tests for forge.ClusterClient's HOST projection.

Everything else ClusterClient derives assumes the consumer is a POD:
`config_json`'s kubeconfigPath is `mount_path + "/" + key`, which is a
path that exists only inside a container, because it is where `.volume`
mounts `.secret`.

A HOST process reading the same CLUSTER_CONFIGS needs a path on the
developer's disk, and there is no volume to derive one from. forge's own
HostDeploy runs services on the host in dev, so before this a dev stack
whose host service had to reach a second cluster had NO declarative way
to say where its kubeconfig lived — the workaround being a hardcoded
`~/.kube/config` literal in an env's main.k, which works on one machine,
enters version control, and defeats the drift-prevention this schema
exists for.

Exercises:
  * host_config_json carries the SAME id and context as the pod form, so
    the two cannot disagree about WHICH cluster they name.
  * the host path is the one declared, not derived from mount_path.
  * omitting host_kubeconfig_path leaves host_config_json Undefined
    rather than falling back to the pod path.
"""

import forge
import json

_cp = forge.Cluster { name = "control-plane", config = "deploy/k3d.yaml", ingress = True }
_daemon = forge.Cluster { name = "cp-daemon", owner = _cp, api_port = 6444 }

# A HOST process (forge.HostDeploy in dev) reaching into cp-daemon. Same
# id, same cluster refs, same mount_path as the pod form — only the host
# path is additional.
_host_link = forge.ClusterClient {
    id = "cp-daemon"
    cluster = _daemon
    in_cluster = _cp
    mount_path = "/etc/daemon-cluster"
    host_kubeconfig_path = "/Users/dev/.kube/cp-daemon.yaml"
}

# The same client WITHOUT a host path — the default, and every existing
# consumer.
_pod_link = forge.ClusterClient {
    id = "cp-daemon"
    cluster = _daemon
    in_cluster = _cp
    mount_path = "/etc/daemon-cluster"
}

# The host form names the declared path, NOT the pod mount path.
_host_cfg = json.decode(_host_link.host_config_json)
assert_host_len = len(_host_cfg) == 1
assert_host_path = _host_cfg[0].kubeconfigPath == "/Users/dev/.kube/cp-daemon.yaml"

# THE LOAD-BEARING ASSERTION: id and context are the SAME in both forms.
# Only the file location differs, which is the one thing that genuinely
# does. If these could drift, a host process and a pod would believe they
# were addressing the same cluster while naming different contexts.
_pod_cfg = json.decode(_host_link.config_json)
assert_same_id = _host_cfg[0].id == _pod_cfg[0].id
assert_same_context = _host_cfg[0].context == _pod_cfg[0].context
assert_paths_differ = _host_cfg[0].kubeconfigPath != _pod_cfg[0].kubeconfigPath

# The pod projection is untouched by the new field — existing consumers
# render exactly as before.
assert_pod_path_unchanged = _pod_cfg[0].kubeconfigPath == "/etc/daemon-cluster/kubeconfig"

# NO FALLBACK. Unset means Undefined, not the pod path: handing a host
# process `/etc/daemon-cluster/kubeconfig` would fail as an unhelpful
# "no configuration has been provided" from client-go rather than as the
# missing declaration it actually is.
assert_no_host_json = _pod_link.host_config_json == Undefined
assert_no_host_entry = _pod_link.host_config_entry == Undefined

# The volume and secret are unaffected in both forms.
assert_vol_unchanged = _host_link.volume.mount_path == "/etc/daemon-cluster"
assert_secret_unchanged = _host_link.secret.context_name == "cp-daemon"
