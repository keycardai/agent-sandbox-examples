# The Slack agent on sandbox-base. `sandbox run` builds it from this
# directory as ghcr.io/keycardai/sandbox-slack, tagged with a digest of the
# directory, and with BASE_IMAGE set to the sandbox-base image the pod would
# run. The agent needs only the python3
# in sandbox-base.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

COPY slack-agent /usr/local/bin/slack-agent
