import { useState, useEffect, useRef } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { FixedSizeList as List } from 'react-window'
import { 
  ArrowLeft, 
  Search, 
  Filter, 
  Play, 
  Pause, 
  Download,
  AlertCircle,
  Clock,
  Terminal,
  X,
  Copy,
  ChevronDown
} from 'lucide-react'
import { getProcess, getProcessLogs } from '../api/processes'
import { formatDistanceToNow } from 'date-fns'

export default function ProcessDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [searchQuery, setSearchQuery] = useState('')
  const [streamFilter, setStreamFilter] = useState('all')
  const [isStreaming, setIsStreaming] = useState(true)
  const [logs, setLogs] = useState([])
  const [modalLog, setModalLog] = useState(null)
  const [showModal, setShowModal] = useState(false)
  const [isAtBottom, setIsAtBottom] = useState(true)
  const listRef = useRef()
  const eventSourceRef = useRef()

  // Fetch process metadata
  const { data: process, isLoading, error } = useQuery({
    queryKey: ['process', id],
    queryFn: () => getProcess(id),
  })

  // Fetch initial logs
  const { data: initialLogs } = useQuery({
    queryKey: ['process-logs', id],
    queryFn: () => getProcessLogs(id, { limit: 'unlimited' }),
    enabled: !!id,
  })

  // Set initial logs
  useEffect(() => {
    if (initialLogs?.logs) {
      setLogs(initialLogs.logs)
    }
  }, [initialLogs])

  // Set up SSE for live logs
  useEffect(() => {
    if (!isStreaming || !id) return

    const eventSource = new EventSource(`/api/processes/${id}/logs/stream`)
    eventSourceRef.current = eventSource

    eventSource.onmessage = (event) => {
      const data = JSON.parse(event.data)
      if (data.type === 'connected') return

      setLogs(prevLogs => {
        const newLogs = [...prevLogs, data]
        
        // Performance optimization: limit in-memory logs to prevent UI slowdown
        const MAX_LOGS_IN_MEMORY = 20000;
        const finalLogs = newLogs.length > MAX_LOGS_IN_MEMORY 
          ? newLogs.slice(-MAX_LOGS_IN_MEMORY) // Keep only the most recent logs
          : newLogs;
        
        // Auto-scroll to bottom only if user was already at bottom
        if (listRef.current && isAtBottom) {
          setTimeout(() => {
            listRef.current.scrollToItem(finalLogs.length - 1, 'end')
          }, 10)
        }
        return finalLogs
      })
    }

    eventSource.onerror = (error) => {
      console.error('SSE error:', error)
    }

    return () => {
      eventSource.close()
    }
  }, [id, isStreaming])

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (eventSourceRef.current) {
        eventSourceRef.current.close()
      }
    }
  }, [])

  // Periodic health check for running processes
  useEffect(() => {
    if (!id || !process || process.status !== 'running') return

    const checkHealth = async () => {
      try {
        const response = await fetch(`/api/processes/${id}/health`)
        const healthData = await response.json()
        
        // If process is no longer healthy, refetch the process data to update status
        if (!healthData.is_healthy) {
          console.log(`Process ${id} is no longer healthy: ${healthData.reason}`)
          // Force refetch of process data to get updated status
          window.location.reload()
        }
      } catch (error) {
        console.error('Health check failed:', error)
      }
    }

    // Check health every 10 seconds for running processes
    const healthCheckInterval = setInterval(checkHealth, 10000)
    
    // Initial health check after 5 seconds
    const initialCheck = setTimeout(checkHealth, 5000)

    return () => {
      clearInterval(healthCheckInterval)
      clearTimeout(initialCheck)
    }
  }, [id, process])

  // Add keyboard event listener for ESC key
  useEffect(() => {
    const handleKeyDown = (event) => {
      if (event.key === 'Escape' && showModal) {
        closeLogModal()
      }
    }

    if (showModal) {
      document.addEventListener('keydown', handleKeyDown)
      return () => document.removeEventListener('keydown', handleKeyDown)
    }
  }, [showModal])

  // Filter logs based on search and stream type
  const filteredLogs = logs.filter(log => {
    const matchesSearch = !searchQuery || 
      log.message.toLowerCase().includes(searchQuery.toLowerCase())
    const matchesStream = streamFilter === 'all' || log.stream === streamFilter
    return matchesSearch && matchesStream
  })

  // Function to sanitize log messages
  const sanitizeLogMessage = (message) => {
    if (!message || typeof message !== 'string') return '';
    
    // Remove ANSI escape sequences
    const ansiRegex = /\x1b\[[0-9;]*[a-zA-Z]/g;
    let sanitized = message.replace(ansiRegex, '');
    
    // Replace control characters (except newlines and tabs) with spaces
    sanitized = sanitized.replace(/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/g, ' ');
    
    // Normalize multiple consecutive whitespace characters
    sanitized = sanitized.replace(/\s+/g, ' ');
    
    // Trim excessive whitespace
    sanitized = sanitized.trim();
    
    return sanitized;
  };

  const openLogModal = (log, index) => {
    setModalLog({ ...log, index, sanitizedMessage: sanitizeLogMessage(log.message) })
    setShowModal(true)
  }

  const closeLogModal = () => {
    setShowModal(false)
    setModalLog(null)
  }

  const copyLogToClipboard = () => {
    if (modalLog) {
      navigator.clipboard.writeText(modalLog.sanitizedMessage)
    }
  }

  const scrollToBottom = () => {
    if (listRef.current && filteredLogs.length > 0) {
      listRef.current.scrollToItem(filteredLogs.length - 1, 'end')
      setIsAtBottom(true)
    }
  }

  // Handle scroll events to detect if user is at bottom
  const handleScroll = ({ scrollOffset, scrollDirection }) => {
    if (!filteredLogs.length) return
    
    const itemHeight = 60
    const containerHeight = 384
    const totalHeight = filteredLogs.length * itemHeight
    const maxScrollOffset = Math.max(0, totalHeight - containerHeight)
    const threshold = 50 // pixels from bottom
    
    setIsAtBottom(scrollOffset >= maxScrollOffset - threshold)
  }

  const LogRow = ({ index, style }) => {
    const log = filteredLogs[index]
    const timestamp = new Date(log.timestamp).toLocaleTimeString()
    const sanitizedMessage = sanitizeLogMessage(log.message)
    const isLongMessage = sanitizedMessage.length > 150
    const displayMessage = isLongMessage 
      ? sanitizedMessage.substring(0, 150) + '...' 
      : sanitizedMessage
    
    return (
      <div 
        style={style} 
        className={`log-line ${log.stream === 'stderr' ? 'log-stderr' : 'log-stdout'} cursor-pointer hover:bg-gray-100 transition-colors`}
        title="Click to view full log in modal"
        onClick={() => openLogModal(log, index)}
      >
        <div className="flex items-start space-x-2">
          <span className="text-xs text-gray-400 font-mono w-20 flex-shrink-0">
            {timestamp}
          </span>
          <span className={`text-xs px-1 rounded text-white font-mono w-12 text-center flex-shrink-0 ${
            log.stream === 'stderr' ? 'bg-red-500' : 'bg-blue-500'
          }`}>
            {log.stream}
          </span>
          <span className="flex-1 font-mono text-sm overflow-hidden whitespace-nowrap text-ellipsis">
            {displayMessage}
            {isLongMessage && (
              <span className="text-gray-400 text-xs ml-2">
                (truncated)
              </span>
            )}
          </span>
        </div>
      </div>
    )
  }

  const downloadLogs = () => {
    const logContent = filteredLogs
      .map(log => `[${log.timestamp}] [${log.stream}] ${log.message}`)
      .join('\n')
    
    const blob = new Blob([logContent], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${process?.name || 'process'}-logs.txt`
    a.click()
    URL.revokeObjectURL(url)
  }

  if (isLoading) {
    return (
      <div className="flex justify-center items-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary-600"></div>
      </div>
    )
  }

  if (error || !process) {
    return (
      <div className="text-center py-12">
        <AlertCircle className="h-12 w-12 text-red-500 mx-auto mb-4" />
        <p className="text-red-600 mb-4">
          {error?.message || 'Process not found'}
        </p>
        <button 
          onClick={() => navigate('/')}
          className="btn-primary"
        >
          Back to Processes
        </button>
      </div>
    )
  }

  const formatDuration = (seconds) => {
    if (seconds < 60) return `${Math.round(seconds)}s`
    if (seconds < 3600) return `${Math.round(seconds / 60)}m`
    return `${Math.round(seconds / 3600)}h`
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center space-x-4">
          <Link 
            to="/" 
            className="text-gray-600 hover:text-gray-800 flex items-center"
          >
            <ArrowLeft className="h-5 w-5 mr-1" />
            Back to Processes
          </Link>
          <h1 className="text-2xl font-bold text-gray-900">
            {process.name || 'Unnamed Process'}
          </h1>
        </div>
      </div>

      {/* Process Metadata */}
      <div className="bg-white rounded-lg border border-gray-200 p-6">
        <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
          <div>
            <h3 className="text-sm font-medium text-gray-500 mb-2">Status</h3>
            <div className={`status-badge status-${process.status} inline-flex items-center`}>
              <span className="capitalize">{process.status}</span>
              {process.exit_code !== null && (
                <span className="ml-1">(exit: {process.exit_code})</span>
              )}
            </div>
          </div>
          
          <div>
            <h3 className="text-sm font-medium text-gray-500 mb-2">Duration</h3>
            <div className="flex items-center text-gray-900">
              <Clock className="h-4 w-4 mr-1" />
              {formatDuration(process.duration)}
            </div>
          </div>

          <div>
            <h3 className="text-sm font-medium text-gray-500 mb-2">Started</h3>
            <div className="text-gray-900">
              {formatDistanceToNow(new Date(process.start_time), { addSuffix: true })}
            </div>
          </div>
        </div>

        <div className="mt-6">
          <h3 className="text-sm font-medium text-gray-500 mb-2">Command</h3>
          <div className="bg-gray-100 rounded-lg p-3 font-mono text-sm">
            {process.command}
          </div>
        </div>

        {process.tags && process.tags.length > 0 && (
          <div className="mt-4">
            <h3 className="text-sm font-medium text-gray-500 mb-2">Tags</h3>
            <div className="flex flex-wrap gap-2">
              {process.tags.map((tag, index) => (
                <span
                  key={index}
                  className="inline-flex items-center px-2 py-1 rounded-full text-xs bg-primary-100 text-primary-800"
                >
                  {tag}
                </span>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* Log Controls */}
      <div className="bg-white rounded-lg border border-gray-200 p-4">
        <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between space-y-4 sm:space-y-0">
          <div className="flex items-center space-x-4">
            {/* Search */}
            <div className="relative">
              <Search className="h-4 w-4 absolute left-3 top-3 text-gray-400" />
              <input
                type="text"
                placeholder="Search logs..."
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                className="pl-10 pr-4 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-primary-500 focus:border-primary-500"
              />
            </div>

            {/* Stream Filter */}
            <div className="flex items-center space-x-2">
              <Filter className="h-4 w-4 text-gray-500" />
              <select
                value={streamFilter}
                onChange={(e) => setStreamFilter(e.target.value)}
                className="border border-gray-300 rounded-lg px-3 py-2 focus:ring-2 focus:ring-primary-500 focus:border-primary-500"
              >
                <option value="all">All Streams</option>
                <option value="stdout">stdout only</option>
                <option value="stderr">stderr only</option>
              </select>
            </div>
          </div>

          <div className="flex items-center space-x-2">
            {/* Streaming Toggle */}
            <button
              onClick={() => setIsStreaming(!isStreaming)}
              className={`flex items-center px-3 py-2 rounded-lg transition-colors ${
                isStreaming 
                  ? 'bg-red-100 text-red-800 hover:bg-red-200' 
                  : 'bg-green-100 text-green-800 hover:bg-green-200'
              }`}
            >
              {isStreaming ? (
                <>
                  <Pause className="h-4 w-4 mr-1" />
                  Pause Live
                </>
              ) : (
                <>
                  <Play className="h-4 w-4 mr-1" />
                  Resume Live
                </>
              )}
            </button>

            {/* Download Logs */}
            <button
              onClick={downloadLogs}
              className="flex items-center px-3 py-2 bg-gray-100 text-gray-800 rounded-lg hover:bg-gray-200 transition-colors"
            >
              <Download className="h-4 w-4 mr-1" />
              Download
            </button>

            {/* Scroll to Bottom */}
            <button
              onClick={scrollToBottom}
              className="flex items-center px-3 py-2 bg-primary-100 text-primary-800 rounded-lg hover:bg-primary-200 transition-colors"
              title="Scroll to bottom of logs"
            >
              <ChevronDown className="h-4 w-4 mr-1" />
              Bottom
            </button>
          </div>
        </div>

        <div className="mt-4 text-sm text-gray-600">
          Showing {filteredLogs.length} of {logs.length} log entries
          {logs.length >= 20000 && (
            <span className="ml-2 text-orange-600">
              (Limited to most recent 20k entries for performance)
            </span>
          )}
        </div>
      </div>

      {/* Log Viewer */}
      <div className="bg-white rounded-lg border border-gray-200">
        <div className="p-4 border-b border-gray-200 bg-gray-50">
          <div className="flex items-center">
            <Terminal className="h-5 w-5 text-gray-500 mr-2" />
            <span className="font-medium text-gray-900">Process Logs</span>
            {isStreaming && (
              <div className="ml-2 flex items-center">
                <div className="h-2 w-2 bg-green-400 rounded-full animate-pulse"></div>
                <span className="ml-1 text-xs text-green-600">Live</span>
              </div>
            )}
          </div>
        </div>
        
        <div className="h-96">
          {filteredLogs.length === 0 ? (
            <div className="flex items-center justify-center h-full text-gray-500">
              <div className="text-center">
                <Terminal className="h-8 w-8 mx-auto mb-2" />
                <p>No logs found</p>
                {searchQuery && <p className="text-sm">Try adjusting your search filters</p>}
              </div>
            </div>
          ) : (
            <List
              ref={listRef}
              height={384}
              itemCount={filteredLogs.length}
              itemSize={60}
              initialScrollOffset={filteredLogs.length * 60}
              onScroll={handleScroll}
            >
              {LogRow}
            </List>
          )}
        </div>
      </div>

      {/* Log Modal */}
      {showModal && modalLog && (
        <div className="fixed inset-0 bg-black bg-opacity-50 flex items-center justify-center p-4 z-50" onClick={closeLogModal}>
          <div className="bg-white rounded-lg shadow-xl max-w-4xl w-full max-h-[80vh] flex flex-col" onClick={e => e.stopPropagation()}>
            {/* Modal Header */}
            <div className="flex items-center justify-between p-4 border-b border-gray-200">
              <div className="flex items-center space-x-4">
                <Terminal className="h-5 w-5 text-gray-500" />
                <div>
                  <h3 className="text-lg font-semibold text-gray-900">Log Details</h3>
                  <div className="flex items-center space-x-2 text-sm text-gray-500">
                    <span>Line #{modalLog.index + 1}</span>
                    <span>•</span>
                    <span>{new Date(modalLog.timestamp).toLocaleString()}</span>
                    <span>•</span>
                    <span className={`px-2 py-1 rounded text-xs text-white ${
                      modalLog.stream === 'stderr' ? 'bg-red-500' : 'bg-blue-500'
                    }`}>
                      {modalLog.stream}
                    </span>
                  </div>
                </div>
              </div>
              <div className="flex items-center space-x-2">
                <button
                  onClick={copyLogToClipboard}
                  className="flex items-center px-3 py-2 bg-gray-100 text-gray-700 rounded-lg hover:bg-gray-200 transition-colors"
                >
                  <Copy className="h-4 w-4 mr-1" />
                  Copy
                </button>
                <button
                  onClick={closeLogModal}
                  className="p-2 text-gray-500 hover:text-gray-700 hover:bg-gray-100 rounded-lg transition-colors"
                >
                  <X className="h-5 w-5" />
                </button>
              </div>
            </div>
            
            {/* Modal Content */}
            <div className="flex-1 p-4 overflow-auto">
              <pre className="font-mono text-sm bg-gray-50 p-4 rounded-lg whitespace-pre-wrap break-words text-gray-900 leading-relaxed">
{modalLog.sanitizedMessage}
              </pre>
            </div>
            
            {/* Modal Footer */}
            <div className="px-4 py-3 border-t border-gray-200 bg-gray-50 text-xs text-gray-500 rounded-b-lg">
              Character count: {modalLog.sanitizedMessage.length} | Click outside or press ESC to close
            </div>
          </div>
        </div>
      )}
    </div>
  )
}