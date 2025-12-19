from sqlalchemy import Column, String, Float, DateTime, Integer
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.sql import func

from backend.common.db import Base


class MetricRaw(Base):
    __tablename__ = "metrics_raw"

    id = Column(Integer, primary_key=True, index=True)

    # Multi-tenancy
    tenant_id = Column(String, index=True, nullable=False)

    # Source context
    source = Column(String, nullable=False)
    service = Column(String, index=True, nullable=False)
    resource_id = Column(String, index=True, nullable=False)
    metric_name = Column(String, index=True, nullable=False)

    # Value
    value = Column(Float, nullable=False)
    unit = Column(String, nullable=True)

    # Time
    timestamp = Column(DateTime(timezone=True), index=True, nullable=False)
    received_at = Column(
        DateTime(timezone=True),
        server_default=func.now(),
        nullable=False,
    )

    # Flexible metadata
    tags = Column(JSONB, nullable=False)
