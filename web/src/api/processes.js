import axios from 'axios'

// Auto-detect API base URL based on environment
const getApiBase = () => {
  // If running on zrok domain, use the API tunnel URL from environment
  if (window.location.hostname.includes('zrok.io')) {
    const apiUrl = import.meta.env.VITE_API_URL
    if (!apiUrl) {
      console.error('VITE_API_URL not set! Please set it in .env.local')
      alert('Please set VITE_API_URL in .env.local file with your API tunnel URL')
    }
    return apiUrl || 'http://localhost:3001'
  }
  // Local development
  return 'http://localhost:3001'
}

const API_BASE = getApiBase()

console.log('API_BASE:', API_BASE) // Debug log

const api = axios.create({
  baseURL: API_BASE,
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