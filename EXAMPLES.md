# LogRun Example Usage

Here are some examples of using the LogRun CLI:

## Basic Usage

```bash
# Wrap any command
./bin/logrun npm run build
./bin/logrun python script.py
./bin/logrun make test

# With custom name
./bin/logrun --name "frontend-build" npm run build

# With tags for organization
./bin/logrun --tags "build,frontend,ci" npm run build

# Run in specific directory
./bin/logrun --cwd /path/to/project npm install

# Set environment variables
./bin/logrun --env NODE_ENV=production --env PORT=8080 npm start

# Run in background (detached)
./bin/logrun --detach --name "dev-server" npm run dev

# Custom API server
./bin/logrun --api-url http://remote-server:3001 python train.py
```

## Development Workflow

```bash
# Start all services
./dev.sh

# In another terminal, run some commands
./bin/logrun --name "test-suite" pytest tests/
./bin/logrun --name "linter" eslint src/
./bin/logrun --tags "build,production" npm run build:prod
```

## Use Cases

### CI/CD Pipeline
```bash
./bin/logrun --name "install-deps" --tags "ci,setup" npm ci
./bin/logrun --name "run-tests" --tags "ci,test" npm test
./bin/logrun --name "build-app" --tags "ci,build" npm run build
./bin/logrun --name "deploy" --tags "ci,deploy" npm run deploy
```

### Data Processing
```bash
./bin/logrun --name "data-ingestion" --tags "etl,daily" python ingest_data.py
./bin/logrun --name "data-processing" --tags "etl,daily" python process_data.py
./bin/logrun --name "data-export" --tags "etl,daily" python export_data.py
```

### Development Tasks
```bash
./bin/logrun --name "database-migration" python manage.py migrate
./bin/logrun --name "dev-server" --detach npm run dev
./bin/logrun --name "watch-tests" --detach npm run test:watch
```

Then view all processes and logs in the web interface at http://localhost:3000