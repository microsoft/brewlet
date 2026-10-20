"""Real-node read-only runtime and mount-policy enforcement, including admission loss."""

import copy
import json

from harness import require


PROBE = r"""
import errno, json, os, stat
assert os.getuid() != 0 and os.getgid() != 0
assert stat.S_IMODE(os.stat('/tmp').st_mode) == 0o1777
denied = {}
for directory in ('/tmp', '/app'):
    try:
        with open(directory + '/brewlet-filesystem-probe', 'w') as target:
            target.write('must not succeed')
    except OSError as error:
        assert error.errno == errno.EROFS, (directory, error.errno)
        denied[directory] = error.errno
    else:
        raise AssertionError('runtime/application filesystem was writable')
with open('/app/new-volume-target/writable', 'w') as target:
    target.write('explicit volume')
print(json.dumps({'uid': os.getuid(), 'gid': os.getgid(), 'denied': denied,
                  'volume': open('/app/new-volume-target/writable').read()}), flush=True)
"""


def execute(h):
    require("python" in h.languages, "filesystem proof requires the explicit Python fixture")
    def delete(name):
        h.delete("pod", name, h.namespace)
        h.wait("filesystem Pod deletion: " + name,
               lambda: h.get("pod", name, h.namespace, absent=True) is None)

    executable = h.launch("python")["entrypoint"]
    pod = h.pod("filesystem-initial", "python")
    container = pod["spec"]["containers"][0]
    container.update(command=executable, args=["-c", PROBE])
    container.pop("readinessProbe", None)
    pod["spec"]["restartPolicy"] = "Never"
    h.apply(pod)
    h.wait("initial process proves runtime/app EROFS and writable explicit volume",
           lambda: h.get("pod", "filesystem-initial", h.namespace).get("status", {}).get("phase") == "Succeeded")
    initial = json.loads(h.kubectl("logs", "-n", h.namespace, "filesystem-initial").stdout)
    h.apply(h.pod("filesystem-exec", "python"))
    h.wait("filesystem Exec fixture ready", lambda: h.ready("filesystem-exec"))
    executed = json.loads(h.exec("filesystem-exec", [*executable, "-c", PROBE]).stdout)
    require(initial == executed and initial["volume"] == "explicit volume",
            "initial/Exec filesystem or identity behavior differs")
    h.record("runtime world-writable /tmp and packaged /app reject writes; explicit volume works in initial and Exec",
             {"initial": initial, "exec": executed}, ["FS-01", "FS-03"])
    for name in ("filesystem-initial", "filesystem-exec"):
        delete(name)

    temporary = h.pod("filesystem-temp", "python")
    temporary["spec"]["volumes"].append({"name": "tmp", "emptyDir": {"sizeLimit": "32Mi"}})
    container = temporary["spec"]["containers"][0]
    container["volumeMounts"].append({"name": "tmp", "mountPath": "/tmp"})
    container.update(command=executable, args=["-c",
        "from pathlib import Path; p=Path('/tmp/permitted'); p.write_text('explicit temporary volume'); "
        "print(p.read_text(), flush=True)"])
    container.pop("readinessProbe", None)
    temporary["spec"]["restartPolicy"] = "Never"
    h.apply(temporary)
    h.wait("platform-approved temporary volume is writable", lambda:
           h.get("pod", "filesystem-temp", h.namespace).get("status", {}).get("phase") == "Succeeded")
    require(h.kubectl("logs", "-n", h.namespace, "filesystem-temp").stdout.strip() ==
            "explicit temporary volume", "approved temporary mount did not work")
    delete("filesystem-temp")
    h.record("nonempty platform mount policy permits explicit bounded /tmp storage",
             {"volumeMountPaths": ["/tmp"], "sizeLimit": "32Mi"}, ["FS-02", "FS-03"])

    cases = []
    writable = h.pod("filesystem-writable", "python", bound=True)
    writable["spec"]["containers"][0]["securityContext"]["readOnlyRootFilesystem"] = False
    cases.append((writable, "ReadOnlyRootRequired"))
    for read_only in (False, True):
        pod = h.pod("filesystem-mount-" + str(read_only).lower(), "python", bound=True)
        pod["spec"]["containers"][0]["volumeMounts"] = [
            {"name": "data", "mountPath": "/usr/lib", "readOnly": read_only}]
        cases.append((pod, "RuntimeMountNotAllowed"))
    for pod, reason in cases:
        result = h.kubectl("create", "--dry-run=server", "-f", "-", input=json.dumps(pod), check=False)
        require(result.returncode != 0 and reason in result.stderr,
                "admission accepted the prohibited filesystem configuration")
    omitted = h.pod("filesystem-default", "python")
    omitted["spec"]["containers"][0]["securityContext"].pop("readOnlyRootFilesystem")
    admitted = json.loads(h.kubectl("create", "--dry-run=server", "-f", "-", "-o", "json",
                                   input=json.dumps(omitted)).stdout)
    require(admitted["spec"]["containers"][0]["securityContext"]["readOnlyRootFilesystem"] is True,
            "admission did not default the required read-only root")
    h.record("admission defaults immutable roots and rejects writable roots plus read-only/read-write runtime overlays",
             {"denied": [reason for _, reason in cases]}, ["FS-01", "FS-02"])

    h.kubectl("scale", "deployment/brewlet-admission", "-n", h.control_namespace, "--replicas=0")
    try:
        h.wait("private admission deployment stopped", lambda:
               h.get("deployment", "brewlet-admission", h.control_namespace).get("status", {}).get("replicas", 0) == 0)
        for pod, reason in cases:
            pod = copy.deepcopy(pod)
            h.apply(pod)
            h.no_start(pod["metadata"]["name"], reason)
            delete(pod["metadata"]["name"])
        h.record("shim independently refuses writable roots and runtime overlays while admission is unavailable",
                 {"denied": [reason for _, reason in cases]}, ["FS-01", "FS-02"])
    finally:
        h.kubectl("scale", "deployment/brewlet-admission", "-n", h.control_namespace, "--replicas=1")
        h.kubectl("rollout", "status", "deployment/brewlet-admission", "-n", h.control_namespace,
                  "--timeout=180s", timeout=200)
