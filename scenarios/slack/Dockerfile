# The Slack agent on sandbox-base. `sandbox run` builds it from this
# directory as ghcr.io/keycardai/sandbox-slack, with BASE_IMAGE set to the
# sandbox-base image the pod would run. The tag is a digest of BASE_IMAGE
# and the directory. The agent needs only the python3 in sandbox-base.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

COPY slack-agent /usr/local/bin/slack-agent
