# Synthetic stand-in for a ComfyUI comfy_api_nodes module. It is NOT ComfyUI code: it only
# carries the `node_id=` declarations that the catalog's static scan looks for.


class FixtureResolutionNode(IO.ComfyNode):
    @classmethod
    def define_schema(cls):
        return IO.Schema(
            node_id="BriaIncreaseResolution",
            display_name="Fixture: increase resolution",
            category="api node/image/Fixture",
        )


class FixtureSecondNode(IO.ComfyNode):
    @classmethod
    def define_schema(cls):
        return IO.Schema(
            node_id='FixtureSecondNode',
            display_name="Fixture: second node",
        )
