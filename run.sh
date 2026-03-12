#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

echo "Pulling latest changes from git..."
git pull

echo "Building the new image and resolving containers..."
# --build  : Build images before starting containers.
# -d       : Detached mode: Run containers in the background.
docker compose up -d --build

echo "Deployment successful!"
