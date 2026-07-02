"""
Positive test — Service.volumes renders pod-level `spec.volumes` +
container `volumeMounts` AND leaves the restricted-PSA container
securityContext (readOnlyRootFilesystem = true) intact.

Context: forge's restricted-PSA container securityContext sets
`readOnlyRootFilesystem = true`. A service that writes to disk (e.g.
writing TLS certs to /data) dies at runtime with
"mkdir data: read-only file system" because there is no writable path.
`Service.volumes = [Volume{type="empty_dir", mount_path="/data"}]` is the
escape hatch: the empty_dir volume mounted at /data is writable, while
the root FS stays read-only — security baseline unchanged, only the
mounted path is writable.

This test pins:

  * an empty_dir volume → pod `volumes[*].emptyDir == {}` + a container
    `volumeMounts[*]` at /data (writable; no readOnly).
  * a config_map volume (read_only=True) → pod
    `volumes[*].configMap.name` + a read-only container mount with the
    declared subPath behaviour.
  * the container STILL has readOnlyRootFilesystem == true — proving the
    emptyDir is the writable escape, not a securityContext relaxation.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
}

# The exact shape a consumer writes — empty_dir at /data (writable
# escape) + a read-only config_map projection at /etc/app.
_svc = forge.K8sWorkload {
    name = "certwriter"
    image = "certwriter-img"
    deploy = _target.deploy | {ports = [8080]}
    volumes = [
        forge.Volume {
            name = "data"
            mount_path = "/data"
            type = "empty_dir"
        }
        forge.Volume {
            name = "cfg"
            mount_path = "/etc/app"
            type = "config_map"
            source = "app-config"
            read_only = True
        }
    ]
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [_svc]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

# Pull the certwriter Deployment's pod spec + container.
_dep = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "certwriter"][0]
_pod_spec = _dep.spec.template.spec
_ctr = _pod_spec.containers[0]

# ── Pod-level spec.volumes ─────────────────────────────────────────

_vols = _pod_spec.volumes
assert_two_volumes = len(_vols) == 2

# empty_dir volume → {name, emptyDir: {}}.
_data_vol = [v for v in _vols if v.name == "data"][0]
assert_data_emptydir = _data_vol.emptyDir == {}
assert_data_no_configmap = "configMap" not in _data_vol

# config_map volume → {name, configMap: {name: <source>}}.
_cfg_vol = [v for v in _vols if v.name == "cfg"][0]
assert_cfg_configmap = _cfg_vol.configMap.name == "app-config"
assert_cfg_no_emptydir = "emptyDir" not in _cfg_vol

# ── Container volumeMounts ─────────────────────────────────────────

_mounts = _ctr.volumeMounts
assert_two_mounts = len(_mounts) == 2

# Writable empty_dir mount at /data — readOnly OMITTED (not set), so the
# path is writable.
_data_mount = [m for m in _mounts if m.name == "data"][0]
assert_data_mount_path = _data_mount.mountPath == "/data"
assert_data_mount_writable = "readOnly" not in _data_mount
assert_data_mount_no_subpath = "subPath" not in _data_mount

# Read-only config_map mount at /etc/app.
_cfg_mount = [m for m in _mounts if m.name == "cfg"][0]
assert_cfg_mount_path = _cfg_mount.mountPath == "/etc/app"
assert_cfg_mount_readonly = _cfg_mount.readOnly == True

# ── The securityContext baseline is UNCHANGED ──────────────────────
# This is the whole point: /data is writable via the emptyDir mount,
# while the container root FS stays read-only.
assert_ro_rootfs_intact = _ctr.securityContext.readOnlyRootFilesystem == True
assert_nonroot_intact = _pod_spec.securityContext.runAsNonRoot == True
assert_drop_all_intact = _ctr.securityContext.capabilities.drop == ["ALL"]
