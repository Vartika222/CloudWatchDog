from fastapi import Header, HTTPException, status
from pydantic import BaseModel


class RequestContext(BaseModel):
    tenant_id: str
    user_id: str | None = None
    role: str | None = None


async def get_request_context(
    x_tenant_id: str = Header(..., alias="X-Tenant-ID"),
) -> RequestContext:
    """
    TEMPORARY:
    Use X-Tenant-ID header for tenant identification.

    Later:
    This will be replaced by JWT / OAuth-based auth.
    """
    if not x_tenant_id:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="Missing X-Tenant-ID header",
        )

    return RequestContext(
        tenant_id=x_tenant_id,
        user_id=None,
        role=None,
    )
