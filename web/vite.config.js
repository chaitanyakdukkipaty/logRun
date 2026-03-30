import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import fs from 'fs'
import path from 'path'

// Proxy target: use tunnel URL if set (so the proxy forwards to wherever the
// API actually is), otherwise fall back to the local default.
const API_URL = process.env.VITE_API_URL || 'http://localhost:3001'
const PORT    = parseInt(process.env.VITE_PORT || '3000', 10)

// Vite plugin: write the web port to .logrun-services.json on server start
// so the CLI and other instances can discover it regardless of what port is used.
function logrunServiceStatePlugin() {
  return {
    name: 'logrun-service-state',
    configureServer(server) {
      server.httpServer?.once('listening', () => {
        try {
          const addr = server.httpServer.address()
          const webPort = typeof addr === 'object' ? addr.port : PORT
          const stateFile = path.resolve(__dirname, '..', '.logrun-services.json')
          let state = {}
          try { state = JSON.parse(fs.readFileSync(stateFile, 'utf8')) } catch (_) {}
          state.web_port = webPort
          state.updated_at = new Date().toISOString()
          fs.writeFileSync(stateFile, JSON.stringify(state, null, 2))
          console.log(`[logrun] service state written → ${stateFile}`)
        } catch (err) {
          console.warn('[logrun] could not write service state file:', err.message)
        }
      })
    }
  }
}

export default defineConfig({
  plugins: [react(), logrunServiceStatePlugin()],
  server: {
    port: PORT,
    host: '0.0.0.0',
    allowedHosts: [
      'localhost',
      '.zrok.io',
      '.ngrok.io',
      '.ngrok-free.app',
      '.ngrok.app',
    ],
    proxy: {
      '/api': {
        target: API_URL,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, '')
      }
    }
  }
})