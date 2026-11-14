#!/usr/bin/env sh
# Install runlet as a systemd --user service. No root is needed.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR="${HOME}/.local/bin"
UNIT_DIR="${HOME}/.config/systemd/user"
CONF_DIR="${HOME}/.config/runlet"

if [ ! -x "${ROOT}/bin/runlet" ]; then
  echo "bin/runlet not found; run: make build" >&2
  exit 1
fi

mkdir -p "${BIN_DIR}" "${UNIT_DIR}" "${CONF_DIR}"
install -m 0755 "${ROOT}/bin/runlet" "${BIN_DIR}/runlet"
install -m 0644 "${ROOT}/scripts/systemd/runlet.service" "${UNIT_DIR}/runlet.service"

if [ ! -f "${CONF_DIR}/config.yaml" ]; then
  install -m 0600 "${ROOT}/runlet.example.yaml" "${CONF_DIR}/config.yaml"
  echo "wrote ${CONF_DIR}/config.yaml; edit it before starting"
fi

if [ ! -f "${CONF_DIR}/env" ]; then
  install -m 0600 /dev/null "${CONF_DIR}/env"
  echo "wrote an empty ${CONF_DIR}/env; add GITHUB_TOKEN=..."
fi

if command -v loginctl >/dev/null 2>&1; then
  linger="$(loginctl show-user "${USER}" -p Linger --value 2>/dev/null || true)"
  if [ "${linger}" != "yes" ]; then
    echo "warning: user linger is off; run: loginctl enable-linger ${USER}" >&2
  fi
fi

systemctl --user daemon-reload
systemctl --user enable runlet.service
echo "installed. Start with: systemctl --user start runlet.service"
