from fastapi import FastAPI

from backend.common.config import settings
from backend.common.db import Base, engine
from backend.services.processing import models as processing_models  
from backend.services.processing.router import router as processing_router
from backend.services.ingestion.read_router import router as metrics_read_router



from backend.services.ingestion import models  # noqa: F401
from backend.services.ingestion.router import router as ingestion_router


def create_app() -> FastAPI:
    app = FastAPI(title=settings.PROJECT_NAME)

    Base.metadata.create_all(bind=engine)

    app.include_router(
        ingestion_router,
        prefix=settings.API_V1_PREFIX,
    )

    app.include_router(
    processing_router,
    prefix=settings.API_V1_PREFIX,
)

    app.include_router(
    metrics_read_router,
    prefix=settings.API_V1_PREFIX,
)


    @app.get("/healthz")
    def health():
        return {"status": "ok"}

    return app


app = create_app()
