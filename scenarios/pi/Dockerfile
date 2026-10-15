# The pi coding agent on sandbox-base. `sandbox run` builds it from this
# directory as ghcr.io/keycardai/sandbox-pi, with BASE_IMAGE set to the
# sandbox-base image the pod would run. The tag is a digest of BASE_IMAGE
# and the directory. Each download waits at most 20 seconds for the server
# and retries at most twice.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root

# pi needs Node.js 22.19 or newer, and sandbox-base has Debian's Node 20.
# The checksums come from https://nodejs.org/dist/v24.21.0/SHASUMS256.txt.
ARG NODE_VERSION=24.21.0
RUN arch=$(dpkg --print-architecture) \
	&& case "$arch" in \
		amd64) node_arch=x64 sum=6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff ;; \
		arm64) node_arch=arm64 sum=724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5 ;; \
		*) echo "no Node.js checksum for $arch" >&2; exit 1 ;; \
	esac \
	&& curl -fsSL --connect-timeout 20 --retry 2 -o /tmp/node.tar.gz \
		"https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.gz" \
	&& echo "$sum  /tmp/node.tar.gz" | sha256sum -c - \
	&& mkdir /opt/node \
	&& tar -xzf /tmp/node.tar.gz -C /opt/node --strip-components=1 --no-same-owner \
	&& rm /tmp/node.tar.gz
ENV PATH=/opt/node/bin:$PATH

# pi's find and grep tools run fd and ripgrep. PI_OFFLINE stops pi from
# downloading them, so the image installs Debian's. pi also finds fd under
# Debian's name, fdfind. The agent cannot install packages in the sandbox,
# whose proxy allows only the demo's hosts, so the image also has jq, make,
# patch, unzip, tree and less.
RUN apt-get -o Acquire::http::Timeout=20 -o Acquire::Retries=2 update \
	&& apt-get -o Acquire::http::Timeout=20 -o Acquire::Retries=2 install --yes --no-install-recommends \
		fd-find ripgrep jq make patch unzip tree less \
	&& rm -rf /var/lib/apt/lists/*

# package-lock.json pins pi and every package it depends on, with their
# integrity hashes. --ignore-scripts skips every install script.
COPY package.json package-lock.json /opt/pi/
RUN cd /opt/pi \
	&& npm ci --ignore-scripts --omit=dev --no-audit --no-fund --fetch-timeout=20000 \
	&& ln -s /opt/pi/node_modules/.bin/pi /usr/local/bin/pi
COPY pi-session /usr/local/bin/pi-session

# sandbox-base's agent uid. The charts' agent.uid matches it.
USER 1000:1000
