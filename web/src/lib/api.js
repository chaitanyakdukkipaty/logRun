import axios from 'axios'

const api = axios.create({
  baseURL: '/api',
  timeout: 10000,
})

// Process APIs
export const processApi = {
  getAll: () => api.get('/processes'),
  getById: (id) => api.get(`/processes/${id}`),
  delete: (id) => api.delete(`/processes/${id}`),
}

// Logs APIs
export const logsApi = {
  getProcessLogs: (id, params = {}) => api.get(`/processes/${id}/logs`, { params }),
  streamProcessLogs: (id) => {
    // Return EventSource for SSE
    return new EventSource(`/api/processes/${id}/logs/stream`)
  },
}

export default api