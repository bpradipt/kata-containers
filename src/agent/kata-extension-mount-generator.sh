#!/bin/bash
#
# Copyright (c) 2026 NVIDIA Corporation
#
# SPDX-License-Identifier: Apache-2.0
#
# systemd system generator that instantiates the kata-extension-mount@.service
# template (and the kata-image-cache-setup@.service template) for every guest
# extension image declared on the kernel command line.
#
# The runtime emits one "kata.extension.<name>.verity_params=..." entry per
# configured guest_extension_images, so this generator enables exactly the
# extensions the VM is configured with -- without the rootfs build needing to
# know any extension name in advance. Adding a new extension therefore requires
# no change to the image build: it is wired up purely from the kernel cmdline.
#
# For each extension the generator wires two units:
#   * kata-extension-mount@<name>.service mounts the extension read-only.
#   * kata-image-cache-setup@<name>.service binds the extension's embedded image
#     cache (if any) into CDH's work directory. It no-ops for extensions that
#     carry no meta_store.json.
#
# systemd invokes generators with three directory arguments; $1 is the "normal"
# output directory, which is where dependency symlinks belong.

set -u

normal_dir="${1:-/tmp}"
mount_unit="/usr/lib/systemd/system/kata-extension-mount@.service"
cache_setup_unit="/usr/lib/systemd/system/kata-image-cache-setup@.service"
target="kata-containers.target"
wants_dir="${normal_dir}/${target}.wants"

# /proc/cmdline is a single line; read it defensively so a missing /proc does
# not abort the generator (which would only delay the boot, never fix it).
read -r cmdline < /proc/cmdline || exit 0

for param in ${cmdline}; do
	# An extension may carry verity params ("...verity_params=<...>") or none
	# at all -- an unmeasured extension (e.g. on s390x) renders as a bare
	# "...verity_params" with no value. Match both forms so the extension is
	# activated either way; the mount helper decides verity vs. raw from the
	# parameter value.
	case "${param}" in
		kata.extension.*.verity_params=*)
			name="${param#kata.extension.}"
			name="${name%%.verity_params=*}"
			;;
		kata.extension.*.verity_params)
			name="${param#kata.extension.}"
			name="${name%.verity_params}"
			;;
		*)
			continue
			;;
	esac

	# Only accept names safe to use as a systemd instance and as a filename.
	# A stray space or '=' would mean a malformed cmdline entry; skip it rather
	# than create a bogus unit instance.
	[[ "${name}" =~ ^[a-zA-Z0-9_-]+$ ]] || continue

	mkdir -p "${wants_dir}"
	ln -sf "${mount_unit}" "${wants_dir}/kata-extension-mount@${name}.service"
	ln -sf "${cache_setup_unit}" "${wants_dir}/kata-image-cache-setup@${name}.service"
done

exit 0
