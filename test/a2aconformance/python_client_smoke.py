#!/usr/bin/env python3
"""Independent A2A Python SDK smoke against the Kyber conformance fixture."""

import asyncio
import sys

import httpx

from a2a.client import A2ACardResolver, ClientConfig, ClientFactory
from a2a.types import (
    GetTaskRequest,
    ListTasksRequest,
    Message,
    Part,
    Role,
    SendMessageConfiguration,
    SendMessageRequest,
)
from a2a.utils.constants import TransportProtocol


async def main(base_url: str) -> None:
    async with httpx.AsyncClient(timeout=10.0) as http:
        card = await A2ACardResolver(http, base_url).get_agent_card()
        assert len(card.supported_interfaces) == 1
        assert card.supported_interfaces[0].protocol_binding == TransportProtocol.HTTP_JSON
        client = ClientFactory(ClientConfig(
            httpx_client=http,
            supported_protocol_bindings=[TransportProtocol.HTTP_JSON],
            streaming=False,
        )).create(card)
        request = SendMessageRequest(
            message=Message(
                message_id="python-sdk-smoke",
                role=Role.ROLE_USER,
                parts=[Part(text="Independent Python client smoke")],
            ),
            configuration=SendMessageConfiguration(return_immediately=True),
        )
        responses = [response async for response in client.send_message(request)]
        assert len(responses) == 1 and responses[0].HasField("task")
        task_id = responses[0].task.id
        assert task_id and responses[0].task.context_id
        task = await client.get_task(GetTaskRequest(id=task_id))
        assert task.id == task_id
        page = await client.list_tasks(ListTasksRequest(page_size=10))
        assert any(item.id == task_id for item in page.tasks)
        await client.close()


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1]))
