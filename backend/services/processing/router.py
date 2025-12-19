from fastapi import APIRouter, Depends
from sqlalchemy.orm import Session

from backend.common.db import get_db
from backend.common.deps import get_request_context, RequestContext
from .runner import run_simple_anomaly_detection

router = APIRouter(prefix="/process", tags=["processing"])


@router.post("/run")
def run_processing(
    ctx: RequestContext = Depends(get_request_context),
    db: Session = Depends(get_db),
):
    run_simple_anomaly_detection(db, tenant_id=ctx.tenant_id)
    return {"status": "processing completed"}
