#!/usr/bin/env bash
# Download and unzip the MTA GTFS Static feed into data/gtfs_static/.
# Idempotent: skips download if files exist and are less than 7 days old.
set -euo pipefail

DEST_DIR="data/gtfs_static"
ZIP_FILE="/tmp/google_transit.zip"
URL="http://web.mta.info/developers/data/nyct/subway/google_transit.zip"
MAX_AGE_DAYS=7

# If stops.txt exists and is recent enough, skip.
if [[ -f "${DEST_DIR}/stops.txt" ]]; then
  age=$(( ($(date +%s) - $(stat -f %m "${DEST_DIR}/stops.txt")) / 86400 ))
  if (( age < MAX_AGE_DAYS )); then
    echo "GTFS static data is ${age} day(s) old (< ${MAX_AGE_DAYS}), skipping download."
    exit 0
  fi
  echo "GTFS static data is ${age} day(s) old, re-downloading."
fi

mkdir -p "${DEST_DIR}"
echo "Downloading GTFS static feed from ${URL} ..."
curl -fSL -o "${ZIP_FILE}" "${URL}"
echo "Unzipping into ${DEST_DIR} ..."
unzip -o "${ZIP_FILE}" -d "${DEST_DIR}"
rm -f "${ZIP_FILE}"
echo "Done. Files in ${DEST_DIR}:"
ls -1 "${DEST_DIR}"
