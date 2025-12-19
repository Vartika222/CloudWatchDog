# CloudWatchDog

CloudWatchDog is a multi-tenant cloud observability backend that ingests time-series metrics, detects anomalies using explainable statistical logic, and exposes queryable APIs for monitoring system health.

---

## Core Features (V1)

- Multi-tenant metric ingestion
- Time-series storage in PostgreSQL
- Deterministic anomaly detection (no ML)
- Cold-start–safe processing logic
- Explainable baselines and deviations
- Read APIs for metrics and anomalies

---

## Architecture (V1)

Client / Agent
|
| POST /api/v1/ingest/metrics
v
FastAPI Ingestion Layer
|
v
PostgreSQL (metrics_raw)
|
| POST /api/v1/process/run
v
Processing Engine
|
v
PostgreSQL (anomalies_raw)
|
+--> GET /api/v1/metrics/timeseries
+--> GET /api/v1/anomalies


---

## Tech Stack

- FastAPI
- PostgreSQL
- SQLAlchemy
- Pydantic

---

## Running Locally

### 1. Install dependencies
```bash
pip install -r requirements.txt
2. Create environment file
DATABASE_URL=postgresql://localhost:5432/cloudwatchdog


Save this as .env.

Running Locally
1. Install dependencies
pip install -r requirements.txt

2. Create environment file

Create a .env file in the project root:

DATABASE_URL=postgresql://localhost:5432/cloudwatchdog


Note: .env is intentionally not committed to version control.

3. Start PostgreSQL
brew services start postgresql@15


Ensure the database exists:

createdb cloudwatchdog

4. Run the server
uvicorn backend.main:app --reload


Health check:

http://127.0.0.1:8000/healthz

Example Usage
Ingest metrics
curl -X POST "http://127.0.0.1:8000/api/v1/ingest/metrics" \
  -H "Content-Type: application/json" \
  -H "X-Tenant-ID: tenant-1" \
  -d '{
    "events": [
      {
        "source": "simulator",
        "service": "ec2",
        "resource_id": "i-test123",
        "metric_name": "cpu_utilization",
        "value": 180.0,
        "timestamp": "2025-01-01T00:00:00Z",
        "tags": { "env": "dev" }
      }
    ]
  }'

Run anomaly processing
curl -X POST "http://127.0.0.1:8000/api/v1/process/run" \
  -H "X-Tenant-ID: tenant-1"

Read metrics
curl "http://127.0.0.1:8000/api/v1/metrics/timeseries?service=ec2&metric_name=cpu_utilization&minutes=60" \
  -H "X-Tenant-ID: tenant-1"

Read anomalies
curl "http://127.0.0.1:8000/api/v1/anomalies" \
  -H "X-Tenant-ID: tenant-1"
