import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { formatDistanceToNow } from 'date-fns'
import { Play, Square, AlertCircle, Clock, ExternalLink, Trash2, Terminal } from 'lucide-react'
import { getProcesses, deleteProcess } from '../api/processes'

export default function ProcessList() {
  const { data: processes = [], isLoading, error, refetch } = useQuery({
    queryKey: ['processes'],
    queryFn: getProcesses,
    refetchInterval: 5000, // Refresh every 5 seconds
  })

  const handleDelete = async (processId) => {
    if (window.confirm('Are you sure you want to delete this process and its logs?')) {
      try {
        await deleteProcess(processId)
        refetch()
      } catch (error) {
        console.error('Failed to delete process:', error)
        alert('Failed to delete process')
      }
    }
  }

  const getStatusIcon = (status) => {
    switch (status) {
      case 'running':
        return <Play className="h-4 w-4" />
      case 'completed':
        return <Square className="h-4 w-4" />
      case 'failed':
        return <AlertCircle className="h-4 w-4" />
      default:
        return <Clock className="h-4 w-4" />
    }
  }

  const formatDuration = (seconds) => {
    if (seconds < 60) return `${Math.round(seconds)}s`
    if (seconds < 3600) return `${Math.round(seconds / 60)}m`
    return `${Math.round(seconds / 3600)}h`
  }

  if (isLoading) {
    return (
      <div className="flex justify-center items-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary-600"></div>
      </div>
    )
  }

  if (error) {
    return (
      <div className="text-center py-12">
        <AlertCircle className="h-12 w-12 text-red-500 mx-auto mb-4" />
        <p className="text-red-600">Error loading processes: {error.message}</p>
        <button 
          onClick={() => refetch()}
          className="btn-primary mt-4"
        >
          Retry
        </button>
      </div>
    )
  }

  return (
    <div>
      <div className="mb-8">
        <h1 className="text-3xl font-bold text-gray-900">Process Monitor</h1>
        <p className="mt-2 text-gray-600">
          Track and monitor your wrapped processes in real-time
        </p>
      </div>

      {processes.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border border-gray-200">
          <Terminal className="h-12 w-12 text-gray-400 mx-auto mb-4" />
          <h3 className="text-lg font-medium text-gray-900 mb-2">No processes yet</h3>
          <p className="text-gray-600 mb-6">
            Start monitoring processes by running commands with the LogRun CLI
          </p>
          <div className="bg-gray-100 rounded-lg p-4 max-w-md mx-auto">
            <code className="text-sm text-gray-800">
              ./bin/logrun npm run build<br/>
              ./bin/logrun --name "test-suite" pytest
            </code>
          </div>
        </div>
      ) : (
        <div className="bg-white shadow-sm rounded-lg border border-gray-200 overflow-hidden">
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-gray-200">
              <thead className="bg-gray-50">
                <tr>
                  <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Process
                  </th>
                  <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Command
                  </th>
                  <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Status
                  </th>
                  <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Started
                  </th>
                  <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Duration
                  </th>
                  <th className="px-6 py-3 text-right text-xs font-medium text-gray-500 uppercase tracking-wider">
                    Actions
                  </th>
                </tr>
              </thead>
              <tbody className="bg-white divide-y divide-gray-200">
                {processes.map((process) => (
                  <tr key={process.process_id} className="hover:bg-gray-50">
                    <td className="px-6 py-4 whitespace-nowrap">
                      <div className="flex items-center">
                        <div className={`flex-shrink-0 h-2 w-2 rounded-full mr-3 ${
                          process.status === 'running' ? 'bg-blue-400' :
                          process.status === 'completed' ? 'bg-green-400' :
                          'bg-red-400'
                        }`}></div>
                        <div>
                          <div className="text-sm font-medium text-gray-900">
                            {process.name || 'Unnamed Process'}
                          </div>
                          <div className="text-sm text-gray-500">
                            PID: {process.pid || 'N/A'}
                          </div>
                        </div>
                      </div>
                    </td>
                    <td className="px-6 py-4">
                      <div className="text-sm text-gray-900 font-mono max-w-xs truncate">
                        {process.command}
                      </div>
                      {process.tags && process.tags.length > 0 && (
                        <div className="flex flex-wrap gap-1 mt-1">
                          {process.tags.map((tag, index) => (
                            <span
                              key={index}
                              className="inline-flex items-center px-2 py-0.5 rounded text-xs bg-gray-100 text-gray-800"
                            >
                              {tag}
                            </span>
                          ))}
                        </div>
                      )}
                    </td>
                    <td className="px-6 py-4 whitespace-nowrap">
                      <div className={`status-badge status-${process.status} flex items-center`}>
                        {getStatusIcon(process.status)}
                        <span className="ml-1 capitalize">{process.status}</span>
                        {process.exit_code !== null && (
                          <span className="ml-1">({process.exit_code})</span>
                        )}
                      </div>
                    </td>
                    <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                      {formatDistanceToNow(new Date(process.start_time), { addSuffix: true })}
                    </td>
                    <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                      {formatDuration(process.duration)}
                    </td>
                    <td className="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
                      <div className="flex items-center justify-end space-x-2">
                        <Link
                          to={`/processes/${process.process_id}`}
                          className="text-primary-600 hover:text-primary-900 flex items-center"
                        >
                          <ExternalLink className="h-4 w-4 mr-1" />
                          View
                        </Link>
                        <button
                          onClick={() => handleDelete(process.process_id)}
                          className="text-red-600 hover:text-red-900 flex items-center"
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}