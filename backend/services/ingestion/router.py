from fastapi import APIRouter, Depends, status
from sqlalchemy.orm import Session

from backend.common.db import get_db
from backend.common.deps import get_request_context, RequestContext
from .schemas import MetricBatch
from .models import MetricRaw

router = APIRouter(
    prefix="/ingest",
    tags=["ingestion"],
)


@router.post(
    "/metrics",
    status_code=status.HTTP_202_ACCEPTED,
    summary="Ingest a batch of metric events",
)
def ingest_metrics(
    payload: MetricBatch,
    ctx: RequestContext = Depends(get_request_context),
    db: Session = Depends(get_db),
):
    rows = [
        MetricRaw(
            tenant_id=ctx.tenant_id,
            source=e.source,
            service=e.service,
            resource_id=e.resource_id,
            metric_name=e.metric_name,
            value=e.value,
            unit=e.unit,
            timestamp=e.timestamp,
            tags=e.tags,
        )
        for e in payload.events
    ]

    db.add_all(rows)
    db.commit()

    return {"ingested": len(rows)}
