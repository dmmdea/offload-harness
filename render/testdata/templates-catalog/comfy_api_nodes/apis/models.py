# Synthetic. Pydantic-style model fields also spell `node_id`, but as an annotation
# with no string literal after `=`, so the scan must not take them as node declarations.
from pydantic import BaseModel, Field


class FixtureEnterprise(BaseModel):
    node_id: str = Field(..., description="The enterprise node ID")
    other_node_id: str = Field(default="NotADeclaration")
