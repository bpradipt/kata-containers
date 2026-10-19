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
# The extension itself is mounted (and dm-verity verified) by
# kata-extension-mount@<name>.service before this unit runs, so here we only
# wire its contents into CDH's work directory. This unit runs for every
# extension declared on the kernel command line and exits early (success) for
# extensions that carry no image cache.
#
# Rules enforced here:
#   * The cache extension must be dm-verity verified. image-rs checks image
#     signatures against the manifest digest recorded in meta_store.json and
#     does not re-hash the layers, so an unverified cache is refused.
#   * Only one cache extension per VM: the CDH work directory holds a single
#     meta_store.json and a single numbered layer store.
#   * meta_store.json is installed last. If any step fails, CDH sees no cache
#     and falls back to a registry pull.

set -euo pipefail
shopt -s nullglob

EXTENSION_NAME="${1:?extension name required}"
SRC="/run/kata-extensions/${EXTENSION_NAME}"
# Device created by kata-extension-mount.sh when the extension is verified.
VERITY_DEV="/dev/mapper/extension-${EXTENSION_NAME}"
# Claimed by the first cache extension; mkdir is atomic, so concurrent
# instances of this unit cannot both claim it.
OWNER_DIR="/run/kata-image-cache.owner"

# The CDH image work directory. Its layout is fixed by image-rs and by the
# kata-agent, which launches CDH with this as the work dir. The layer store
# paths recorded in meta_store.json at build time must match these paths, so
# the cache extension must be produced by pre-pulling into this same work dir.
WORK_DIR="/run/kata-containers/image"
LAYERS_DIR="${WORK_DIR}/layers"
META_STORE="${WORK_DIR}/meta_store.json"

die() {
	echo "ERROR: $*" >&2
	exit 1
}

# The presence of meta_store.json marks this extension as an embedded image
# cache. Extensions such as "coco" or "gpu" carry no image cache and are
# skipped, which keeps this unit data-driven rather than name-based.
if [[ ! -f "${SRC}/meta_store.json" ]]; then
	echo "Extension ${EXTENSION_NAME} carries no image cache; nothing to set up"
	exit 0
fi

[[ -b "${VERITY_DEV}" ]] ||
	die "extension ${EXTENSION_NAME} carries an image cache but is not dm-verity verified; refusing to use it"

mkdir "${OWNER_DIR}" 2>/dev/null ||
	die "an image cache is already provided by another extension; ignoring extension ${EXTENSION_NAME}"
echo "${EXTENSION_NAME}" > "${OWNER_DIR}/name"

mkdir -p "${LAYERS_DIR}"

# Bind-mount each embedded layer directory read-only onto the writable layer
# store. image-rs numbers layer directories with increasing integers, and the
# store paths recorded in meta_store.json point at those same integer
# directories under LAYERS_DIR. Bind-mounted directories are valid overlayfs
# lowerdir sources, so image-rs can create container bundle overlays directly
# from them. The parent LAYERS_DIR stays on tmpfs and writable, so a pull for
# an image that is not in the cache still gets fresh (higher-numbered) layer
# directories instead of failing on a read-only store.
for layer in "${SRC}"/layers/*/; do
	n="$(basename "${layer}")"
	# image-rs requires numeric layer directory names in its layer store.
	[[ "${n}" =~ ^[0-9]+$ ]] || die "unexpected layer directory '${n}' in extension ${EXTENSION_NAME}"
	mkdir -p "${LAYERS_DIR}/${n}"
	mount --bind --make-private "${SRC}/layers/${n}" "${LAYERS_DIR}/${n}"
	mount -o remount,ro,bind "${LAYERS_DIR}/${n}"
done

# Install the meta store and bind-mount it to itself. The kata-agent removes
# /run/kata-containers on CreateSandbox, before CDH loads the meta store on its
# first pull; binding the file to itself makes it a mount point, which
# survives that unlink, so CDH can load the embedded reference_db/layer_db.
cp "${SRC}/meta_store.json" "${META_STORE}"
mount --bind --make-private "${META_STORE}" "${META_STORE}"

echo "Embedded image cache from extension ${EXTENSION_NAME} set up at ${WORK_DIR}"
