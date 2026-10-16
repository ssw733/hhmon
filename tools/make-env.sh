#!/usr/bin/env bash
# Создаёт .env с GigaChat-ключом для hhmon.
#
# Использование:
#   tools/make-env.sh CLIENT_ID SECRET
#   tools/make-env.sh                     # спросит интерактивно
#   SBER_CLIENT_ID=... SBER_SECRET=... tools/make-env.sh
#
# Client ID / Secret — из developers.sber.ru (проект GigaChat).

set -euo pipefail
cd "$(dirname "$0")/.."

cid="${1:-${SBER_CLIENT_ID:-}}"
sec="${2:-${SBER_SECRET:-}}"

if [ -z "$cid" ]; then
  read -rp "SBER_CLIENT_ID: " cid
fi
if [ -z "$sec" ]; then
  read -rsp "SBER_SECRET: " sec
  echo
fi

if [ -z "$cid" ] || [ -z "$sec" ]; then
  echo "Ошибка: нужны и CLIENT_ID, и SECRET" >&2
  exit 1
fi

key="$(printf '%s:%s' "$cid" "$sec" | base64 | tr -d '\n')"

umask 077
printf 'GIGACHAT_AUTH_KEY=%s\n' "$key" > .env

echo "Готово: .env создан (GIGACHAT_AUTH_KEY). Файл в .gitignore."
