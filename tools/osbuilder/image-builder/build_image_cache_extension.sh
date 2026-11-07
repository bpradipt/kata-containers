#!/usr/bin/env bash
#
# Copyright (c) 2026 Kata Containers contributors
#
# SPDX-License-Identifier: Apache-2.0
#
# Build a guest extension image that carries an embedded container image cache
# (pre-pulled OCI layers + meta_store.json) for kata-image-cache-setup@.service.
#
# The images are pre-pulled with the guest-components cdh-oneshot binary into
# the same work directory CDH uses in the guest (/run/kata-containers/image), so
# the layer store paths recorded in meta_store.json resolve at runtime. The
# result is packed by image_builder.sh as an EROFS + dm-verity extension; the
# guest refuses an image cache extension that is not dm-verity verified.
#
# Usage:
#   sudo CDH_ONESHOT=/path/to/cdh-oneshot \
#     build_image_cache_extension.sh -o <output.img> <image-ref> [<image-ref>...]
#
# Environment:
#   CDH_ONESHOT    path to the cdh-oneshot binary built from a guest-components
#                  revision that supports enable_embedded_image_cache (required)
#   BUILD_VARIANT  name of the root hash file, root_hash_<variant>.txt
#                  (default: imgcache-extension)
#   USE_DOCKER     passed through to image_builder.sh
#
# Security: dm-verity protects integrity only. The layers are stored
# decrypted in the extension image, so do not embed images that contain
# secrets. Images with encrypted layers are never recorded in the cache.

set -o errexit
set -o nounset
set -o pipefail

script_dir="$(dirname "$(readlink -f "$0")")"
readonly script_dir
readonly work_dir="/run/kata-containers/image"

die() {
	echo "ERROR: $*" >&2
	exit 1
}

usage() {
	echo "Usage: CDH_ONESHOT=<cdh-oneshot> $0 -o <output.img> <image-ref> [<image-ref>...]"
	exit "${1:-0}"
}

output=""
while getopts "ho:" opt; do
	case "${opt}" in
		h) usage 0 ;;
		o) output="${OPTARG}" ;;
		*) usage 1 ;;
	esac
done
shift $((OPTIND - 1))

[[ -n "${output}" ]] || usage 1
[[ $# -gt 0 ]] || usage 1
[[ "$(id -u)" -eq 0 ]] || die "must run as root (pre-pulling mounts tmpfs and overlayfs)"
[[ -x "${CDH_ONESHOT:-}" ]] || die "CDH_ONESHOT must point to an executable cdh-oneshot binary"

staging="$(mktemp -d)"
cdh_config="$(mktemp)"
trap 'rm -rf "${staging}" "${cdh_config}"' EXIT

cat > "${cdh_config}" <<EOF
[kbc]
name = "offline_fs_kbc"
url = ""

[image]
work_dir = "${work_dir}"
enable_embedded_image_cache = true
EOF

# Pre-pull in a private mount namespace, so the tmpfs over /run/kata-containers
# never hides the build host's own /run/kata-containers (for example a host
# running kata). All mounts disappear when the namespace exits.
mkdir -p /run/kata-containers
unshare --mount --propagation private bash -o errexit -o nounset -o pipefail -c '
	oneshot="$1" cfg="$2" work_dir="$3" staging="$4"
	shift 4
	mount -t tmpfs tmpfs /run/kata-containers
	i=0
	for img in "$@"; do
		bundle="/run/kata-containers/bundle.${i}"
		mkdir -p "${bundle}"
		echo "pre-pulling ${img}"
		"${oneshot}" --config "${cfg}" pull-image --image-url "${img}" --bundle-path "${bundle}"
		i=$((i + 1))
	done
	# Fail when nothing was recorded, e.g. for images with encrypted layers.
	grep -q "\"reference_db\":{\"" "${work_dir}/meta_store.json" ||
		{ echo "ERROR: no image was recorded in reference_db" >&2; exit 1; }
	cp -a "${work_dir}/layers" "${staging}/layers"
	cp "${work_dir}/meta_store.json" "${staging}/meta_store.json"
' prepull "${CDH_ONESHOT}" "${cdh_config}" "${work_dir}" "${staging}" "$@"

BUILD_VARIANT="${BUILD_VARIANT:-imgcache-extension}" \
	FS_TYPE="erofs" \
	MEASURED_ROOTFS="yes" \
	SKIP_DAX_HEADER="yes" \
	SKIP_ROOTFS_CHECK="yes" \
	"${script_dir}/image_builder.sh" -o "${output}" "${staging}"

echo "Built ${output}"
echo "Verity parameters: $(dirname "${output}")/root_hash_${BUILD_VARIANT:-imgcache-extension}.txt"
