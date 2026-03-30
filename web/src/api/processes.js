// processes.js intentionally uses relative /api paths so all requests go
// through the Vite dev-server proxy (see vite.config.js). This means the
// same code works for both local access and remote tunnel access (zrok/ngrok)
// because the proxy runs server-side on the local machine.
import axios from 'axios'

const api = axios.create({
  baseURL: '/api',
  timeout: 30000,
})

// Process API functions
export const getProcesses = async () => {
  const response = await api.get('/processes')
  return response.data
}

export const getProcess = async (id) => {
  const response = await api.get(`/processes/${id}`)
  return response.data
}

export const deleteProcess = async (id) => {
  const response = await api.delete(`/processes/${id}`)
  return response.data
}

export const getProcessLogs = async (id, params = {}) => {
  const response = await api.get(`/processes/${id}/logs`, { params })
  return response.data
}

export default api