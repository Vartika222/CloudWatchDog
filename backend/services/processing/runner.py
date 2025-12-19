from sqlalchemy.orm import Session
from sqlalchemy import func
from datetime import timedelta

from backend.services.ingestion.models import MetricRaw
from .models import AnomalyRaw


MIN_POINTS = 5
THRESHOLD_MULTIPLIER = 1.5


def run_simple_anomaly_detection(
    db: Session,
    tenant_id: str,
    window_minutes: int = 10,
):
    """
    V1 anomaly detection logic:
    - requires minimum data points
    - baseline excludes latest point
    - simple, explainable thresholding
    """

    window_start = func.now() - timedelta(minutes=window_minutes)

    # Step 1: group metrics
    metric_groups = (
        db.query(
            MetricRaw.service,
            MetricRaw.resource_id,
            MetricRaw.metric_name,
        )
        .filter(
            MetricRaw.tenant_id == tenant_id,
            MetricRaw.timestamp >= window_start,
        )
        .group_by(
            MetricRaw.service,
            MetricRaw.resource_id,
            MetricRaw.metric_name,
        )
        .all()
    )

    for service, resource_id, metric_name in metric_groups:

        # Step 2: fetch recent points
        points = (
            db.query(MetricRaw)
            .filter(
                MetricRaw.tenant_id == tenant_id,
                MetricRaw.service == service,
                MetricRaw.resource_id == resource_id,
                MetricRaw.metric_name == metric_name,
                MetricRaw.timestamp >= window_start,
            )
            .order_by(MetricRaw.timestamp.asc())
            .all()
        )

        if len(points) < MIN_POINTS:
            continue  # cold start protection

        latest = points[-1]
        historical = points[:-1]

        # Step 3: compute baseline from history only
        baseline = sum(p.value for p in historical) / len(historical)

        deviation = abs(latest.value - baseline)

        # Step 4: threshold check
        if deviation > THRESHOLD_MULTIPLIER * baseline:
            severity = "high" if deviation > 2.5 * baseline else "medium"

            anomaly = AnomalyRaw(
                tenant_id=tenant_id,
                service=service,
                resource_id=resource_id,
                metric_name=metric_name,
                value=latest.value,
                baseline=baseline,
                deviation=deviation,
                severity=severity,
                timestamp=latest.timestamp,
            )

            db.add(anomaly)

    db.commit()
