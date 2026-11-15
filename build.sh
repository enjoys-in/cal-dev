#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

# Configuration
IMAGE_NAME="caldev-server"
CONTAINER_NAME="caldev-server"
ENV_FILE=".env"
DEFAULT_PORT="8443"
BIND_IP="127.0.0.1" # Accessible only on localhost, never exposed publicly
RESTART_POLICY="unless-stopped"

# Ensure .env file exists
if [ ! -f "$ENV_FILE" ]; then
    if [ -f ".env.example" ]; then
        echo "⚠️  Warning: $ENV_FILE not found. Creating from .env.example..."
        cp .env.example "$ENV_FILE"
    else
        echo "❌ Error: $ENV_FILE file not found!"
        exit 1
    fi
fi

# Extract port from .env if defined, otherwise fallback to DEFAULT_PORT
PORT=$(grep -E '^SERVER_PORT=' "$ENV_FILE" 2>/dev/null | cut -d '=' -f2 | tr -d ' "\r')
PORT=${PORT:-$DEFAULT_PORT}

echo "=========================================="
echo " Starting Build & Deploy: $CONTAINER_NAME"
echo "=========================================="

# 1. First build the new Docker image
echo "🔨 Step 1: Building new Docker image '$IMAGE_NAME'..."
docker build -t "$IMAGE_NAME" .

echo "✅ Docker image built successfully."

# 2. Stop and remove the old container
echo "🛑 Step 2: Stopping old container '$CONTAINER_NAME'..."
docker stop "$CONTAINER_NAME" 2>/dev/null || true

echo "🗑️  Removing old container '$CONTAINER_NAME'..."
docker rm "$CONTAINER_NAME" 2>/dev/null || true

# 3. Run the new container using the environment file (bound to localhost only)
echo "🚀 Step 3: Running new container '$CONTAINER_NAME' on $BIND_IP:$PORT (localhost only)..."
docker run -d \
    --name "$CONTAINER_NAME" \
    --env-file "$ENV_FILE" \
    --restart "$RESTART_POLICY" \
    --add-host=host.docker.internal:host-gateway \
    -p "$BIND_IP:$PORT:$PORT" \
    "$IMAGE_NAME"

echo "=========================================="
echo "✅ $CONTAINER_NAME is up and running on $BIND_IP:$PORT (localhost only)!"
echo "=========================================="
docker ps --filter "name=$CONTAINER_NAME"
