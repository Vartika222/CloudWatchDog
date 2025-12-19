from sqlalchemy import Column, String, Float, DateTime, Integer
from sqlalchemy.sql import func

from backend.common.db import Base


class AnomalyRaw(Base):
    __tablename__ = "anomalies_raw"

    id = Column(Integer, primary_key=True)

    tenant_id = Column(String, index=True, nullable=False)
    service = Column(String, index=True, nullable=False)
    resource_id = Column(String, index=True, nullable=False)
    metric_name = Column(String, index=True, nullable=False)

    value = Column(Float, nullable=False)
    baseline = Column(Float, nullable=False)
    deviation = Column(Float, nullable=False)

    severity = Column(String, nullable=False)

    timestamp = Column(DateTime(timezone=True), index=True, nullable=False)
    detected_at = Column(
        DateTime(timezone=True),
        server_default=func.now(),
        nullable=False,
    )
