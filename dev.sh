#!/bin/bash

# Development script for LogRun project
set -e

echo "🚀 Starting LogRun in development mode..."

# Function to kill background processes on exit
cleanup() {
    echo "🛑 Stopping services..."
    kill $API_PID 2>/dev/null || true
    kill $WEB_PID 2>/dev/null || true
    exit 0
}

# Set up cleanup trap
trap cleanup SIGINT SIGTERM

# Build CLI if not exists
if [ ! -f "bin/logrun" ]; then
    echo "📦 Building CLI..."
    cd cmd/logrun
    go mod tidy
    go build -o ../../bin/logrun .
    cd ../..
    echo "✅ CLI built -> bin/logrun"
fi

# Install dependencies if needed
if [ ! -d "api/node_modules" ]; then
    echo "📦 Installing API dependencies..."
    cd api && npm install && cd ..
fi

if [ ! -d "web/node_modules" ]; then
    echo "📦 Installing Web UI dependencies..."
    cd web && npm install && cd ..
fi

echo "🚀 Starting API server..."
cd api
npm start &
API_PID=$!
cd ..

echo "🚀 Starting Web UI..."
cd web
npm run dev &
WEB_PID=$!
cd ..

echo ""
echo "✅ LogRun services started:"
echo "  📡 API Server: http://localhost:3001"
echo "  🌐 Web UI: http://localhost:3000"
echo "  🖥️  CLI: ./bin/logrun [command]"
echo ""
echo "Press Ctrl+C to stop all services"

# Wait for background processes
wait