const express = require('express');
const sqlite3 = require('sqlite3').verbose();
const cors = require('cors');
const compression = require('compression');
const helmet = require('helmet');
const morgan = require('morgan');
const path = require('path');
const fs = require('fs-extra');
const readline = require('readline');
const chokidar = require('chokidar');

const app = express();
const PORT = process.env.PORT || 3001;

// Queue Configuration
const QUEUE_CONFIG = {
  maxSize: 10000,           // Maximum queue size
  workerPoolSize: 5,        // Number of workers
  batchSize: 100,           // Logs per batch for DB operations
  flushInterval: 1000,      // Flush interval in ms
  highWaterMark: 8000,      // Queue depth for backpressure
};

// Queue System
class LogQueue {
  constructor(maxSize = QUEUE_CONFIG.maxSize) {
    this.buffer = [];
    this.maxSize = maxSize;
    this.head = 0;
    this.tail = 0;
    this.size = 0;
  }

  enqueue(item) {
    if (this.size >= this.maxSize) {
      // Drop oldest item (ring buffer behavior)
      this.dequeue();
    }
    
    this.buffer[this.tail] = item;
    this.tail = (this.tail + 1) % this.maxSize;
    this.size++;
    return true;
  }

  dequeue() {
    if (this.size === 0) return null;
    
    const item = this.buffer[this.head];
    this.buffer[this.head] = null; // Clear reference
    this.head = (this.head + 1) % this.maxSize;
    this.size--;
    return item;
  }

  getSize() {
    return this.size;
  }

  isEmpty() {
    return this.size === 0;
  }
}

// Worker Pool
class LogWorkerPool {
  constructor(poolSize, queue, processLogBatch) {
    this.poolSize = poolSize;
    this.queue = queue;
    this.processLogBatch = processLogBatch;
    this.workers = [];
    this.running = false;
  }

  start() {
    this.running = true;
    for (let i = 0; i < this.poolSize; i++) {
      this.workers.push(this.createWorker(i));
    }
    console.log(`Started ${this.poolSize} workers`);
  }

  createWorker(id) {
    const worker = setInterval(async () => {
      if (this.queue.isEmpty()) return;
      
      // Collect batch
      const batch = [];
      while (!this.queue.isEmpty() && batch.length < QUEUE_CONFIG.batchSize) {
        const item = this.queue.dequeue();
        if (item) batch.push(item);
      }

      if (batch.length > 0) {
        try {
          await this.processLogBatch(batch);
        } catch (error) {
          console.error(`Worker ${id} error processing batch:`, error);
        }
      }
    }, 100); // Check every 100ms
    
    return worker;
  }

  stop() {
    this.running = false;
    this.workers.forEach(worker => clearInterval(worker));
    this.workers = [];
  }
}

// Global queue and worker pool
const logQueue = new LogQueue();
let workerPool;

// Middleware
app.use(helmet());
app.use(compression());
app.use(cors({
  origin: ['http://localhost:3000', /\.zrok\.io$/],
  credentials: true
}));
app.use(express.json());
app.use(morgan('combined'));

// Database setup
const DB_PATH = path.join(process.cwd(), '..', 'logrun.db');
const LOGS_DIR = path.join(process.cwd(), '..', 'logs');
let db;

// Initialize database
function initDatabase() {
  db = new sqlite3.Database(DB_PATH, (err) => {
    if (err) {
      console.error('Error opening database:', err.message);
      process.exit(1);
    }
    console.log('Connected to SQLite database');
    
    // Create tables
    createTables((err) => {
      if (err) {
        console.error('Error creating tables:', err);
        process.exit(1);
      }
      console.log('Database tables initialized');
    });
  });
}

// Create database tables
function createTables(callback) {
  const schema = `
    CREATE TABLE IF NOT EXISTS processes (
      process_id TEXT PRIMARY KEY,
      name TEXT,
      command TEXT NOT NULL,
      pid INTEGER,
      status TEXT NOT NULL,
      exit_code INTEGER,
      start_time DATETIME NOT NULL,
      end_time DATETIME,
      tags TEXT,
      cwd TEXT,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_processes_status ON processes(status);
    CREATE INDEX IF NOT EXISTS idx_processes_start_time ON processes(start_time);
    CREATE INDEX IF NOT EXISTS idx_processes_name ON processes(name);
  `;

  db.exec(schema, callback);
}

// Ensure logs directory exists
fs.ensureDirSync(LOGS_DIR);

// SSE connection management
const sseClients = new Map();

// API Routes

// Helper function to broadcast log entries to SSE clients
function broadcastLogEntry(processId, logEntry) {
  let activeClients = 0;
  for (const [clientId, client] of sseClients.entries()) {
    if (client.processId === processId && client.connected && !client.res.destroyed) {
      try {
        client.res.write(`data: ${JSON.stringify(logEntry)}\n\n`);
        client.res.flush(); // Force immediate send
        activeClients++;
      } catch (error) {
        console.error('Error broadcasting to SSE client:', error);
        client.connected = false;
        sseClients.delete(clientId);
      }
    }
  }
  if (activeClients > 0) {
    console.log(`Broadcasted log to ${activeClients} SSE clients for process ${processId}`);
  }
}

// Health check
app.get('/health', (req, res) => {
  res.json({ status: 'OK', timestamp: new Date().toISOString() });
});

// Bulk log processing function
async function processLogBatch(batch) {
  const logWrites = [];
  const sseEvents = [];

  // Group by process_id for efficient processing
  const logsByProcess = {};
  
  for (const item of batch) {
    const { processId, logs } = item;
    if (!logsByProcess[processId]) {
      logsByProcess[processId] = [];
    }
    logsByProcess[processId].push(...logs);
  }

  // Process each process's logs
  for (const [processId, logs] of Object.entries(logsByProcess)) {
    const logPath = path.join(LOGS_DIR, `${processId}.log`);
    
    // Prepare batch write to file
    const logEntries = logs.map(log => JSON.stringify(log) + '\n').join('');
    
    try {
      // Batch write to file
      await fs.appendFile(logPath, logEntries);
      
      // Collect SSE events
      logs.forEach(log => {
        sseEvents.push({ processId, log });
      });
    } catch (error) {
      console.error(`Error writing logs for process ${processId}:`, error);
    }
  }

  // Broadcast all events via SSE
  sseEvents.forEach(({ processId, log }) => {
    broadcastLogEntry(processId, log);
  });
}

// Initialize worker pool
function initWorkerPool() {
  workerPool = new LogWorkerPool(QUEUE_CONFIG.workerPoolSize, logQueue, processLogBatch);
  workerPool.start();
}

// Health check endpoint for queue
app.get('/health/queue', (req, res) => {
  res.json({
    queueDepth: logQueue.getSize(),
    maxQueueSize: QUEUE_CONFIG.maxSize,
    workerPoolSize: QUEUE_CONFIG.workerPoolSize,
    highWaterMark: QUEUE_CONFIG.highWaterMark,
    status: logQueue.getSize() > QUEUE_CONFIG.highWaterMark ? 'PRESSURE' : 'OK'
  });
});

// Batch log ingestion endpoint
app.post('/processes/:id/logs/batch', (req, res) => {
  const processId = req.params.id;
  const { logs } = req.body;

  if (!Array.isArray(logs) || logs.length === 0) {
    return res.status(400).json({ error: 'Invalid logs array' });
  }

  // Check backpressure
  if (logQueue.getSize() > QUEUE_CONFIG.highWaterMark) {
    return res.status(429).json({ 
      error: 'Queue full, try again later',
      retryAfter: 1000,
      queueDepth: logQueue.getSize()
    });
  }

  // Validate log entries
  const validLogs = logs.filter(log => {
    return log.timestamp && log.stream && log.message !== undefined;
  });

  if (validLogs.length === 0) {
    return res.status(400).json({ error: 'No valid log entries found' });
  }

  // Add to queue
  const queueItem = {
    processId,
    logs: validLogs,
    timestamp: new Date().toISOString()
  };

  logQueue.enqueue(queueItem);

  res.status(202).json({ 
    message: 'Logs queued for processing',
    accepted: validLogs.length,
    rejected: logs.length - validLogs.length,
    queueDepth: logQueue.getSize()
  });
});

// Create new process
app.post('/processes', (req, res) => {
  const {
    process_id,
    name,
    command,
    pid,
    status,
    exit_code,
    start_time,
    end_time,
    tags,
    cwd
  } = req.body;

  if (!process_id || !command || !status) {
    return res.status(400).json({ error: 'Missing required fields: process_id, command, status' });
  }

  const query = `
    INSERT INTO processes 
    (process_id, name, command, pid, status, exit_code, start_time, end_time, tags, cwd)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  `;

  const tagsJson = Array.isArray(tags) ? JSON.stringify(tags) : tags || '[]';

  db.run(query, [
    process_id,
    name,
    command,
    pid,
    status,
    exit_code,
    start_time,
    end_time,
    tagsJson,
    cwd
  ], function(err) {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }
    
    res.status(201).json({ 
      message: 'Process created', 
      process_id: process_id 
    });
  });
});

// Update existing process
app.put('/processes/:id', (req, res) => {
  const processId = req.params.id;
  const {
    name,
    command,
    pid,
    status,
    exit_code,
    start_time,
    end_time,
    tags,
    cwd
  } = req.body;

  const query = `
    UPDATE processes SET 
      name = COALESCE(?, name),
      command = COALESCE(?, command),
      pid = COALESCE(?, pid),
      status = COALESCE(?, status),
      exit_code = COALESCE(?, exit_code),
      start_time = COALESCE(?, start_time),
      end_time = COALESCE(?, end_time),
      tags = COALESCE(?, tags),
      cwd = COALESCE(?, cwd)
    WHERE process_id = ?
  `;

  const tagsJson = Array.isArray(tags) ? JSON.stringify(tags) : tags;

  db.run(query, [
    name,
    command,
    pid,
    status,
    exit_code,
    start_time,
    end_time,
    tagsJson,
    cwd,
    processId
  ], function(err) {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }
    
    if (this.changes === 0) {
      return res.status(404).json({ error: 'Process not found' });
    }
    
    res.json({ 
      message: 'Process updated', 
      process_id: processId 
    });
  });
});

// Add log entry for a process
app.post('/processes/:id/logs', async (req, res) => {
  const processId = req.params.id;
  const { timestamp, stream, message } = req.body;

  if (!timestamp || !stream || message === undefined) {
    return res.status(400).json({ error: 'Missing required fields: timestamp, stream, message' });
  }

  const logPath = path.join(LOGS_DIR, `${processId}.log`);
  const logEntry = JSON.stringify({
    process_id: processId,
    timestamp,
    stream,
    message
  }) + '\n';

  try {
    await fs.appendFile(logPath, logEntry);
    res.status(201).json({ message: 'Log entry added' });
    
    // Broadcast to SSE clients
    broadcastLogEntry(processId, { process_id: processId, timestamp, stream, message });
  } catch (error) {
    console.error('Error writing log:', error);
    res.status(500).json({ error: 'Error writing log' });
  }
});
app.get('/processes', (req, res) => {
  const query = `
    SELECT 
      process_id,
      name,
      command,
      pid,
      status,
      exit_code,
      start_time,
      end_time,
      tags,
      cwd,
      created_at
    FROM processes 
    ORDER BY start_time DESC
  `;

  db.all(query, [], (err, rows) => {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }

    const processes = rows.map(row => ({
      ...row,
      tags: JSON.parse(row.tags || '[]'),
      start_time: new Date(row.start_time),
      end_time: row.end_time ? new Date(row.end_time) : null,
      duration: row.end_time ? 
        (new Date(row.end_time) - new Date(row.start_time)) / 1000 : 
        (Date.now() - new Date(row.start_time)) / 1000
    }));

    res.json(processes);
  });
});

// Get specific process
app.get('/processes/:id', (req, res) => {
  const processId = req.params.id;
  
  const query = `
    SELECT 
      process_id,
      name,
      command,
      pid,
      status,
      exit_code,
      start_time,
      end_time,
      tags,
      cwd,
      created_at
    FROM processes 
    WHERE process_id = ?
  `;

  db.get(query, [processId], (err, row) => {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }

    if (!row) {
      return res.status(404).json({ error: 'Process not found' });
    }

    const process = {
      ...row,
      tags: JSON.parse(row.tags || '[]'),
      start_time: new Date(row.start_time),
      end_time: row.end_time ? new Date(row.end_time) : null,
      duration: row.end_time ? 
        (new Date(row.end_time) - new Date(row.start_time)) / 1000 : 
        (Date.now() - new Date(row.start_time)) / 1000
    };

    res.json(process);
  });
});

// Process health check - detect stale running processes
app.get('/processes/:id/health', (req, res) => {
  const processId = req.params.id;
  
  const query = `
    SELECT pid, status, start_time, end_time
    FROM processes 
    WHERE process_id = ?
  `;

  db.get(query, [processId], (err, row) => {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }

    if (!row) {
      return res.status(404).json({ error: 'Process not found' });
    }

    let isHealthy = true;
    let reason = '';

    // If process is marked as running, check if it's actually alive
    if (row.status === 'running' && row.pid) {
      try {
        // Check if process is still running by sending signal 0 (doesn't kill, just checks)
        process.kill(row.pid, 0);
      } catch (error) {
        // Process doesn't exist anymore
        isHealthy = false;
        reason = 'Process no longer exists';
        
        // Auto-update the process status
        const updateQuery = `
          UPDATE processes 
          SET status = 'terminated', end_time = datetime('now') 
          WHERE process_id = ?
        `;
        
        db.run(updateQuery, [processId], (updateErr) => {
          if (updateErr) {
            console.error('Failed to update stale process:', updateErr);
          } else {
            console.log(`Auto-updated stale process ${processId} to terminated`);
          }
        });
      }
    }

    res.json({
      process_id: processId,
      status: row.status,
      pid: row.pid,
      is_healthy: isHealthy,
      reason: reason,
      start_time: row.start_time,
      end_time: row.end_time
    });
  });
});

// Delete process
app.delete('/processes/:id', (req, res) => {
  const processId = req.params.id;
  
  // Delete from database
  db.run('DELETE FROM processes WHERE process_id = ?', [processId], function(err) {
    if (err) {
      console.error('Database error:', err);
      return res.status(500).json({ error: 'Database error' });
    }

    // Delete log file
    const logPath = path.join(LOGS_DIR, `${processId}.log`);
    fs.remove(logPath).catch(console.error);

    res.json({ message: 'Process deleted', deleted_rows: this.changes });
  });
});

// Get process logs
app.get('/processes/:id/logs', async (req, res) => {
  const processId = req.params.id;
  const { q, from, to, stream, limit = 10000, offset = 0 } = req.query;
  
  const logPath = path.join(LOGS_DIR, `${processId}.log`);
  
  try {
    if (!await fs.pathExists(logPath)) {
      return res.status(404).json({ error: 'Log file not found' });
    }

    const logs = await readLogs(logPath, {
      query: q,
      from: from ? new Date(from) : null,
      to: to ? new Date(to) : null,
      stream,
      limit: limit === 'unlimited' ? null : parseInt(limit),
      offset: parseInt(offset)
    });

    res.json({
      logs,
      total: logs.length,
      has_more: logs.length === parseInt(limit)
    });
  } catch (error) {
    console.error('Error reading logs:', error);
    res.status(500).json({ error: 'Error reading logs' });
  }
});

// Stream live logs via SSE
app.get('/processes/:id/logs/stream', (req, res) => {
  const processId = req.params.id;
  
  // Set SSE headers with proper CORS and buffering control
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    'Connection': 'keep-alive',
    'Access-Control-Allow-Origin': '*',
    'Access-Control-Allow-Headers': 'Cache-Control',
    'X-Accel-Buffering': 'no', // Disable proxy buffering
  });

  // Send initial connection message
  res.write('data: {"type":"connected","process_id":"' + processId + '"}\n\n');
  res.flush(); // Force immediate send

  // Store client connection
  const clientId = Date.now() + Math.random();
  const client = { res, processId, connected: true };
  sseClients.set(clientId, client);
  
  console.log(`SSE client connected for process ${processId}`);

  // Watch log file for changes and send existing recent logs
  const logPath = path.join(LOGS_DIR, `${processId}.log`);
  let watcher;
  let lastSize = 0;
  
  if (fs.existsSync(logPath)) {
    // Send recent logs immediately on connection
    readRecentLogs(logPath, 5).then(recentLogs => {
      if (client.connected && !res.destroyed) {
        recentLogs.forEach(log => {
          try {
            res.write(`data: ${JSON.stringify(log)}\n\n`);
            res.flush();
          } catch (error) {
            console.error('Error sending initial logs:', error);
          }
        });
      }
    }).catch(error => {
      console.error('Error reading initial logs:', error);
    });
    
    // Get initial file size
    try {
      const stats = fs.statSync(logPath);
      lastSize = stats.size;
    } catch (error) {
      console.error('Error getting file stats:', error);
    }
    
    watcher = chokidar.watch(logPath, {
      persistent: true,
      usePolling: false, // Use native fs events for better performance
      interval: 100, // Fallback polling interval
      awaitWriteFinish: {
        stabilityThreshold: 50, // Wait for file to be stable for 50ms
        pollInterval: 10
      }
    });
    
    watcher.on('change', async () => {
      try {
        const stats = fs.statSync(logPath);
        if (stats.size > lastSize && client.connected && !res.destroyed) {
          // File grew, read new content
          const recentLogs = await readRecentLogs(logPath, 1);
          recentLogs.forEach(log => {
            try {
              res.write(`data: ${JSON.stringify(log)}\n\n`);
              res.flush();
            } catch (error) {
              console.error('Error streaming log:', error);
              client.connected = false;
            }
          });
          lastSize = stats.size;
        }
      } catch (error) {
        console.error('Error streaming logs:', error);
      }
    });
    
    watcher.on('error', (error) => {
      console.error('File watcher error:', error);
    });
  }

  // Handle client disconnect
  req.on('close', () => {
    console.log(`SSE client disconnected for process ${processId}`);
    client.connected = false;
    sseClients.delete(clientId);
    if (watcher) {
      watcher.close();
    }
  });
  
  req.on('error', (error) => {
    console.error('SSE connection error:', error);
    client.connected = false;
    sseClients.delete(clientId);
    if (watcher) {
      watcher.close();
    }
  });
});

// Helper function to read logs from file
async function readLogs(logPath, filters = {}) {
  const logs = [];
  const fileStream = fs.createReadStream(logPath);
  const rl = readline.createInterface({
    input: fileStream,
    crlfDelay: Infinity
  });

  for await (const line of rl) {
    if (line.trim()) {
      try {
        const log = JSON.parse(line);
        
        // Apply filters
        if (filters.stream && log.stream !== filters.stream) continue;
        if (filters.query && !log.message.toLowerCase().includes(filters.query.toLowerCase())) continue;
        if (filters.from && new Date(log.timestamp) < filters.from) continue;
        if (filters.to && new Date(log.timestamp) > filters.to) continue;
        
        logs.push(log);
      } catch (error) {
        // Skip malformed JSON lines with better debugging
        console.warn(`Malformed log line in ${logPath}:`, JSON.stringify(line.substring(0, 100)), error.message);
      }
    }
  }

  // Apply pagination with safety limits
  if (filters.limit === null) {
    // Unlimited - but cap at 50k logs to prevent memory issues
    const maxUnlimited = 50000;
    const start = filters.offset || 0;
    if (logs.length > maxUnlimited) {
      console.warn(`Large log file detected (${logs.length} entries), limiting to ${maxUnlimited} for unlimited request`);
      return logs.slice(start, start + maxUnlimited);
    }
    return logs.slice(start);
  }
  
  const start = filters.offset || 0;
  const end = start + filters.limit;
  
  return logs.slice(start, end);
}

// Helper function to read recent logs
async function readRecentLogs(logPath, count = 10) {
  const logs = [];
  const fileStream = fs.createReadStream(logPath);
  const rl = readline.createInterface({
    input: fileStream,
    crlfDelay: Infinity
  });

  const allLogs = [];
  for await (const line of rl) {
    if (line.trim()) {
      try {
        allLogs.push(JSON.parse(line));
      } catch (error) {
        console.warn(`Malformed log line in ${logPath}:`, JSON.stringify(line.substring(0, 100)), error.message);
      }
    }
  }

  return allLogs.slice(-count);
}

// Error handling middleware
app.use((error, req, res, next) => {
  console.error('Unhandled error:', error);
  res.status(500).json({ error: 'Internal server error' });
});

// 404 handler
app.use((req, res) => {
  res.status(404).json({ error: 'Endpoint not found' });
});

// Start server
app.listen(PORT, () => {
  console.log(`LogRun API server running on port ${PORT}`);
  initDatabase();
  
  // Initialize queue system
  setTimeout(() => {
    initWorkerPool();
    console.log('Log processing queue system initialized');
  }, 1000); // Wait for DB initialization
});

// Graceful shutdown
process.on('SIGTERM', () => {
  console.log('Received SIGTERM, shutting down gracefully');
  
  // Stop worker pool
  if (workerPool) {
    workerPool.stop();
    console.log('Worker pool stopped');
  }
  
  if (db) {
    db.close((err) => {
      if (err) console.error('Error closing database:', err);
      else console.log('Database connection closed');
    });
  }
  process.exit(0);
});