#!/bin/bash
#
# Copyright (c) 2026 Kata Containers contributors
#
# SPDX-License-Identifier: Apache-2.0
#
# Set up the embedded image cache from a mounted guest extension.
# Usage: kata-image-cache-setup.sh <extension-name>
#
# An extension is treated as an embedded image cache when its root carries a
# meta_store.json (and, usually, a layers/ directory with the pre-pulled OCI
# layers). The layers are bind-mounted read-only into the confidential-data-hub
# (CDH) image work directory so image-rs can serve cached images without
# contacting the registry, while the parent layer store remains writable so
# cache misses can still add new layers.
#
# The extension itself is mounted (and dm-verity verified, when measured) by
# kata-extension-mount@<name>.service before this unit runs, so here we only
# wire its contents into CDH's work directory. This unit runs for every
# extension declared on the kernel command line and exits early (success) for
# extensions that carry no image cache.

set -euo pipefail

EXTENSION_NAME="${1:?extension name required}"
SRC="/run/kata-extensions/${EXTENSION_NAME}"

# The CDH image work directory. Its layout is fixed by image-rs and by the
# kata-agent, which launches CDH with this as the work dir. The layer store
# paths recorded in meta_store.json at build time must match these paths, so
# the cache extension must be produced by pre-pulling into this same work dir.
WORK_DIR="/run/kata-containers/image"
LAYERS_DIR="${WORK_DIR}/layers"
META_STORE="${WORK_DIR}/meta_store.json"

# The presence of meta_store.json marks this extension as an embedded image
# cache. Extensions such as "coco" or "gpu" carry no image cache and are
# skipped, which keeps this unit data-driven rather than name-based.
if [[ ! -f "${SRC}/meta_store.json" ]]; then
	echo "Extension ${EXTENSION_NAME} carries no image cache; nothing to set up"
	exit 0
fi

mkdir -p "${LAYERS_DIR}"

# Bind-mount each embedded layer directory read-only onto the writable layer
# store. image-rs numbers layer directories with increasing integers, and the
# store paths recorded in meta_store.json point at those same integer
# directories under LAYERS_DIR. Bind-mounted directories are valid overlayfs
# lowerdir sources, so image-rs can create container bundle overlays directly
# from them. The parent LAYERS_DIR stays on tmpfs and writable, so a pull for
# an image that is not in the cache still gets fresh (higher-numbered) layer
# directories instead of failing on a read-only store.
if [[ -d "${SRC}/layers" ]]; then
	for layer in "${SRC}"/layers/*/; do
		n="$(basename "${layer}")"
		mkdir -p "${LAYERS_DIR}/${n}"
		mount --bind "${SRC}/layers/${n}" "${LAYERS_DIR}/${n}"
		mount -o remount,ro,bind "${LAYERS_DIR}/${n}"
	done
fi

# Install the meta store and bind-mount it to itself. The kata-agent deletes
# files in the image work directory during guest setup; binding the file to
# itself makes it a mount point, which survives that unlink, so CDH can load
# the embedded reference_db/layer_db on its first pull.
cp "${SRC}/meta_store.json" "${META_STORE}"
mount --bind "${META_STORE}" "${META_STORE}"

echo "Embedded image cache from extension ${EXTENSION_NAME} set up at ${WORK_DIR}"
