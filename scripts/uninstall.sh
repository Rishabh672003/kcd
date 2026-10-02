#!/usr/bin/env bash
# kcd uninstall script
# Usage: ./scripts/uninstall.sh [--purge]
#   --purge  Also remove config, paired devices, and TLS certificates without prompting.
set -euo pipefail

# ── Colour setup ──────────────────────────────────────────────────────────────
if [[ -t 1 ]] && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
  BOLD="$(tput bold)"
  RED="$(tput setaf 1)"
  GREEN="$(tput setaf 2)"
  YELLOW="$(tput setaf 3)"
  BLUE="$(tput setaf 4)"
  CYAN="$(tput setaf 6)"
  RESET="$(tput sgr0)"
else
  BOLD="" RED="" GREEN="" YELLOW="" BLUE="" CYAN="" RESET=""
fi

# ── Logging helpers ───────────────────────────────────────────────────────────
info()    { printf "  ${BLUE}→${RESET}  %s\n"       "$*"; }
success() { printf "  ${GREEN}✓${RESET}  %s\n"      "$*"; }
warn()    { printf "  ${YELLOW}⚠${RESET}  %s\n"     "$*" >&2; }
step()    { printf "\n${BOLD}${CYAN}▶  %s${RESET}\n" "$*"; }
skip()    { printf "  ${YELLOW}–${RESET}  %s\n"      "$*"; }

# ── Argument parsing ──────────────────────────────────────────────────────────
PURGE=false

for arg in "$@"; do
  case "$arg" in
    --purge)   PURGE=true ;;
    --help|-h)
      printf "Usage: %s [--purge]\n" "$(basename "$0")"
      printf "  --purge  Remove config and paired-device state without prompting.\n"
      exit 0 ;;
    *) printf "Unknown argument: %s\n" "$arg" >&2; exit 1 ;;
  esac
done

# ── Paths ─────────────────────────────────────────────────────────────────────
BIN_DIR="${HOME}/.local/bin"
SYSTEMD_DIR="${HOME}/.config/systemd/user"
CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/kcd"
STATE_DIR="${XDG_STATE_HOME:-${HOME}/.local/state}/kcd"
NAUTILUS_EXT_DIR="${HOME}/.local/share/nautilus-python/extensions"

# ── Banner ────────────────────────────────────────────────────────────────────
printf "\n"
printf "${BOLD}${RED}┌─────────────────────────────────────────┐${RESET}\n"
printf "${BOLD}${RED}│${RESET}   ${BOLD}kcd${RESET} — Uninstall                       ${BOLD}${RED}│${RESET}\n"
printf "${BOLD}${RED}└─────────────────────────────────────────┘${RESET}\n"
printf "\n"

# ── Stop and disable service + socket ─────────────────────────────────────────
step "systemd service"

if systemctl --user is-active --quiet kcd.service 2>/dev/null; then
  info "Stopping kcd.service …"
  systemctl --user stop kcd.service
  success "Service stopped"
else
  skip "kcd.service is not running"
fi

if systemctl --user is-active --quiet kcd.socket 2>/dev/null; then
  info "Stopping kcd.socket …"
  systemctl --user stop kcd.socket
  success "Socket stopped"
else
  skip "kcd.socket is not listening"
fi

if systemctl --user is-enabled --quiet kcd.service 2>/dev/null; then
  systemctl --user disable kcd.service
  success "Service disabled"
fi

if systemctl --user is-enabled --quiet kcd.socket 2>/dev/null; then
  systemctl --user disable kcd.socket
  success "Socket disabled"
fi

if [[ -f "${SYSTEMD_DIR}/kcd.service" ]]; then
  rm "${SYSTEMD_DIR}/kcd.service"
  systemctl --user daemon-reload
  success "Removed ${SYSTEMD_DIR}/kcd.service"
else
  skip "No service file found at ${SYSTEMD_DIR}/kcd.service"
fi

if [[ -f "${SYSTEMD_DIR}/kcd.socket" ]]; then
  rm "${SYSTEMD_DIR}/kcd.socket"
  systemctl --user daemon-reload
  success "Removed ${SYSTEMD_DIR}/kcd.socket"
else
  skip "No socket file found at ${SYSTEMD_DIR}/kcd.socket"
fi

# ── Remove binary ─────────────────────────────────────────────────────────────
step "Binary"

if [[ -f "${BIN_DIR}/kcd" ]]; then
  rm "${BIN_DIR}/kcd"
  success "Removed ${BIN_DIR}/kcd"
else
  skip "Binary not found at ${BIN_DIR}/kcd"
fi

if [[ -f "${BIN_DIR}/kcd.backup" ]]; then
  rm "${BIN_DIR}/kcd.backup"
  success "Removed ${BIN_DIR}/kcd.backup"
fi

# ── Shell completions ──────────────────────────────────────────────────────────
step "Shell completions"

BASH_COMP="${HOME}/.local/share/bash-completion/completions/kcd"
ZSH_COMP="${HOME}/.zfunc/_kcd"
FISH_COMP="${HOME}/.config/fish/completions/kcd.fish"

removed_any=false
for f in "${BASH_COMP}" "${ZSH_COMP}" "${FISH_COMP}"; do
  if [[ -f "$f" ]]; then
    rm "$f"
    success "Removed $f"
    removed_any=true
  fi
done
[[ "${removed_any}" == false ]] && skip "No completion files found"

# ── Remove Nautilus extension ──────────────────────────────────────────────────
step "Nautilus extension"

if [[ -f "${NAUTILUS_EXT_DIR}/nautilus-kcd.py" ]]; then
  rm "${NAUTILUS_EXT_DIR}/nautilus-kcd.py"
  success "Removed Nautilus extension"

  if command -v nautilus >/dev/null 2>&1; then
    if [[ -n "${DISPLAY:-}" ]] || [[ -n "${WAYLAND_DISPLAY:-}" ]]; then
      nautilus -q >/dev/null 2>&1 || true
      info "Nautilus restarted"
    fi
  fi
else
  skip "Nautilus extension not installed"
fi

# ── Safe recursive delete ──────────────────────────────────────────────────────
# rm descends into nested filesystems by default, so a live SFTP mount inside
# one of these directories would be traversed and the phone's files deleted.
# --one-file-system makes rm stop at the filesystem boundary instead
# (coreutils >= 8.30 spells it long, BusyBox uses -x).
#
# Probed rather than assumed: on an older coreutils the unrecognized option
# would abort the script under `set -euo pipefail`, leaving a half-uninstalled
# system rather than a merely un-uninstalled one.
if rm --help 2>&1 | grep -q -- '--one-file-system'; then
  KCD_RM_ONE_FS=(--one-file-system)
elif rm --help 2>&1 | grep -qE -- '(^|[[:space:]])-x([[:space:],]|$)'; then
  KCD_RM_ONE_FS=(-x)
else
  KCD_RM_ONE_FS=()
fi

# _rm_rf removes directories without crossing a filesystem boundary when rm
# supports it.
_rm_rf() {
  rm "${KCD_RM_ONE_FS[@]+"${KCD_RM_ONE_FS[@]}"}" -rf "$@"
}

# _is_mountpoint reports whether a directory is itself a mount point.
# Compared by device id, since a mount sits on a different st_dev than its
# parent. This avoids depending on findmnt or mountpoint, which nothing else in
# these scripts uses.
_is_mountpoint() {
  local dir="$1" parent dev_dir dev_parent
  [ -d "${dir}" ] || return 1
  parent="$(dirname -- "${dir}")"
  [ -d "${parent}" ] || return 1
  dev_dir="$(stat -c %d -- "${dir}" 2>/dev/null)" || return 1
  dev_parent="$(stat -c %d -- "${parent}" 2>/dev/null)" || return 1
  [ "${dev_dir}" != "${dev_parent}" ]
}

# _live_sftp_mounts prints any SFTP mount point that is still mounted, under
# the current location or the pre-v1.22 one. Unmounting is the daemon's job, but
# a mount can outlive it -- an unclean shutdown, a crash, or a device id the
# daemon no longer knows -- and this is the last chance to notice.
_live_sftp_mounts() {
  local base dir found=""
  for base in "${STATE_DIR}/mnt" "${HOME}/Downloads/kcd/mnt"; do
    [ -d "${base}" ] || continue
    for dir in "${base}"/kcd-sftp-*; do
      [ -d "${dir}" ] || continue
      # The glob also matches the literal parent when nothing is there.
      case "$(basename -- "${dir}")" in
      kcd-sftp-?*) ;;
      *) continue ;;
      esac
      if _is_mountpoint "${dir}"; then
        found="${found}${dir}"$'\n'
      fi
    done
  done
  printf '%s' "${found}"
}

# ── Config and state ──────────────────────────────────────────────────────────
step "Configuration and state"

_remove_data() {
  local removed=false live

  # Without a boundary-aware rm there is no safe way to delete a directory that
  # still contains a live mount, so refuse rather than delete through it.
  if [ "${#KCD_RM_ONE_FS[@]}" -eq 0 ]; then
    live="$(_live_sftp_mounts)"
    if [ -n "${live}" ]; then
      warn "This system's rm has no --one-file-system, so refusing to delete."
      warn "Unmount the SFTP mount(s) first, then re-run:"
      while IFS= read -r mount_point; do
        [ -n "${mount_point}" ] && warn "  kcd sftp unmount <device-id>   # ${mount_point}"
      done <<<"${live}"
      return 1
    fi
  fi

  if [[ -d "${CONFIG_DIR}" ]]; then
    _rm_rf "${CONFIG_DIR}"
    success "Removed ${CONFIG_DIR}"
    removed=true
  fi
  if [[ -d "${STATE_DIR}" ]]; then
    _rm_rf "${STATE_DIR}"
    success "Removed ${STATE_DIR}  (paired device fingerprints deleted)"
    removed=true
  fi
  if [[ "${removed}" == false ]]; then
    skip "No config or state directories found"
  fi
}

if [[ "${PURGE}" == true ]]; then
  _remove_data
else
  printf "\n"
  printf "  ${YELLOW}Config dir :${RESET}  ${CONFIG_DIR}\n"
  printf "  ${YELLOW}State dir  :${RESET}  ${STATE_DIR}\n"
  printf "\n"
  printf "  ${BOLD}Remove these directories?${RESET}\n"
  printf "  Choosing ${BOLD}yes${RESET} deletes kcd.toml, TLS certificates, and all\n"
  printf "  paired-device fingerprints (you will need to re-pair everything).\n"
  printf "\n"
  read -r -p "  [y/N] → " REPLY
  printf "\n"

  if [[ "${REPLY}" =~ ^[Yy]$ ]]; then
    _remove_data
  else
    skip "Config and state preserved"
    printf "\n"
    printf "  ${BLUE}Tip:${RESET} To remove later, run:\n"
    # Print the boundary-aware form this system actually supports, rather than a
    # bare `rm -rf` that would descend into a live SFTP mount.
    printf "    rm${KCD_RM_ONE_FS[*]:+ ${KCD_RM_ONE_FS[*]}} -rf %s %s\n" \
      "${CONFIG_DIR}" "${STATE_DIR}"
    if [[ "${#KCD_RM_ONE_FS[@]}" -eq 0 ]]; then
      printf "  ${YELLOW}Note:${RESET} this system's rm has no --one-file-system, so\n"
      printf "  unmount any kcd SFTP mount before deleting by hand.\n"
    fi
  fi
fi

# ── Done ──────────────────────────────────────────────────────────────────────
printf "\n"
printf "${BOLD}${GREEN}┌─────────────────────────────────────────┐${RESET}\n"
printf "${BOLD}${GREEN}│${RESET}   ${BOLD}Uninstall complete.${RESET}                   ${BOLD}${GREEN}│${RESET}\n"
printf "${BOLD}${GREEN}└─────────────────────────────────────────┘${RESET}\n"
printf "\n"
