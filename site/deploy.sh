#!/bin/sh
# Rebuild the promo site and restart it on this server.
#   site/deploy.sh
set -eu
cd "$(dirname "$0")/.."
go test ./site
sudo install -d -o azureuser -g azureuser /mnt/data/app/dd-pits-site
go build -trimpath -ldflags "-s -w" -o /mnt/data/app/dd-pits-site/dd-pits-site.new ./site
mv /mnt/data/app/dd-pits-site/dd-pits-site.new /mnt/data/app/dd-pits-site/dd-pits-site
sudo systemctl restart dd-pits-site
sleep 1
curl -fsS http://127.0.0.1:9897/healthz
