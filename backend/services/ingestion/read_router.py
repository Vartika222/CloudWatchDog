from fastapi import APIRouter, Depends, Query
from sqlalchemy.orm import Session
from sqlalchemy import and_
from sqlalchemy import func, text


from backend.common.db import get_db
from backend.common.deps import get_request_context, RequestContext
from .models import MetricRaw

router = APIRouter(prefix="/metrics", tags=["metrics-read"])


@router.get("/timeseries")
def get_metric_timeseries(
    service: str,
    metric_name: str,
    minutes: int = Query(60, ge=1, le=1440),
    ctx: RequestContext = Depends(get_request_context),
    db: Session = Depends(get_db),
):
    """
    Return raw metric values for the last N minutes.
    """

    rows = (
        db.query(
            MetricRaw.timestamp,
            MetricRaw.value,
            MetricRaw.resource_id,
        )
        .filter(
            MetricRaw.tenant_id == ctx.tenant_id,
            MetricRaw.service == service,
            MetricRaw.metric_name == metric_name,
            MetricRaw.timestamp >= func.now() - text(f"interval '{minutes} minutes'")

        )
        .order_by(MetricRaw.timestamp.asc())
        .all()
    )

    return {
        "service": service,
        "metric": metric_name,
        "points": [
            {
                "timestamp": r.timestamp,
                "value": r.value,
                "resource_id": r.resource_id,
            }
            for r in rows
        ],
    }
