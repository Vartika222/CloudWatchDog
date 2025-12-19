from pydantic import BaseModel, Field
from typing import Dict, List, Optional
from datetime import datetime

class MetricEvent(BaseModel):
    source: str = Field(..., example="aws-cloudwatch")
    service: str = Field(..., example="ec2")
    resource_id: str = Field(..., example="i-123456")
    metric_name: str = Field(..., example="cpu_utilization")
    value: float
    unit: Optional[str] = Field(default=None, example="percent")
    timestamp: datetime
    tags: Dict[str, str] = Field(default_factory=dict)

    class Config:
        extra = "ignore"


class MetricBatch(BaseModel):
    events: List[MetricEvent]
