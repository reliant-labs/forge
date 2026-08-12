"""
Negative test — a Job must declare a command.

Every other kind can fall back to the image's default entrypoint, because
"run the server" is what the image does. A one-shot has no such default:
the whole point of the component is the specific thing it runs to
completion. An empty command would render a Job that runs the server
entrypoint and never exits — a one-shot that never finishes, blocking
every dependent forever.
"""

import forge.workloads as fw

_job = fw.Workload {
    kind = "job"
    name = "provision"
    before = ["api"]
}
