import asyncio
import queue
from fastapi import APIRouter, Body, Request, HTTPException, WebSocket, WebSocketDisconnect
import time

from pow.service.manager import PowInitRequestUrl, PowManager
from pow.compute.compute import ProofBatch
from common.logger import create_logger

logger = create_logger(__name__)

API_PREFIX = "/api/v1"

router = APIRouter(
    tags=["API v1"],
)

ws_router = APIRouter(
    tags=["API v1"],
)

@router.post(
    "/pow/init",
    status_code=200,
)
async def init(
    request: Request,
    init_request: PowInitRequestUrl
):
    manager: PowManager = request.app.state.pow_manager
    await manager.switch_to_pow_async(init_request)
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@router.post(
    "/pow/init/generate",
    status_code=200,
)
async def init_generate(
    request: Request,
    init_request: PowInitRequestUrl
):
    if init_request.node_id == -1 or init_request.node_count == -1:
        raise HTTPException(
            status_code=400,
            detail="Node ID and node count must be set"
        )
    manager: PowManager = request.app.state.pow_manager
    if not manager.is_running():
        await manager.switch_to_pow_async(init_request)

    if manager.init_request != init_request:
        await manager.switch_to_pow_async(init_request)

    manager.pow_controller.start_generate()
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@router.post(
    "/pow/init/validate",
    status_code=200,
)
async def init_validate(
    request: Request,
    init_request: PowInitRequestUrl
):
    manager: PowManager = request.app.state.pow_manager
    if not manager.is_running():
        await manager.switch_to_pow_async(init_request)

    if manager.init_request != init_request:
        await manager.switch_to_pow_async(init_request)

    manager.pow_controller.start_validate()
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@router.post(
    "/pow/phase/generate",
    status_code=200,
)
async def start_generate(request: Request):
    manager: PowManager = request.app.state.pow_manager
    if manager.init_request.node_id == -1 or manager.init_request.node_count == -1:
        raise HTTPException(
            status_code=400,
            detail="Node ID and node count must be set to start generating"
        )
    if not manager.is_running():
        raise HTTPException(
            status_code=400,
            detail="PoW is not running"
        )
    manager.pow_controller.start_generate()
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@router.post(
    "/pow/phase/validate",
    status_code=200,
)
async def start_validate(request: Request):
    manager: PowManager = request.app.state.pow_manager
    if not manager.is_running():
        raise HTTPException(
            status_code=400,
            detail="PoW is not running"
        )
    manager.pow_controller.start_validate()
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@router.post(
    "/pow/validate",
    status_code=200,
)
async def validate(
    request: Request,
    proof_batch: ProofBatch = Body(...)
):
    manager: PowManager = request.app.state.pow_manager
    if not manager.is_running():
        raise HTTPException(
            status_code=400,
            detail="PoW is not running"
        )

    manager.pow_controller.to_validate(proof_batch)
    manager.pow_sender.in_validation_queue.put(proof_batch)


@router.get(
    "/pow/status",
    status_code=200,
)
async def status(request: Request):
    manager: PowManager = request.app.state.pow_manager
    return manager.get_pow_status()


@router.post(
    "/pow/stop",
    status_code=200,
)
async def stop(request: Request):
    manager: PowManager = request.app.state.pow_manager
    if not manager.is_running():
        return {
            "status": "OK",
            "pow_status": "PoW is not running"
        }
    manager.stop()
    return {
        "status": "OK",
        "pow_status": manager.get_pow_status()
    }


@ws_router.websocket("/pow/ws")
async def websocket_endpoint(websocket: WebSocket):
    manager: PowManager = websocket.app.state.pow_manager
    
    await websocket.accept()
    
    if not manager.is_running():
        await websocket.close(code=1008, reason="PoW is not running")
        logger.warning("WebSocket connection rejected: PoW is not running")
        return
    
    if not manager.websocket_lock or not manager.websocket_connected:
        await websocket.close(code=1008, reason="WebSocket infrastructure not initialized")
        logger.warning("WebSocket connection rejected: infrastructure not initialized")
        return
    
    with manager.websocket_lock:
        if manager.websocket_connected.value == 1:
            await websocket.close(code=1008, reason="Another client is already connected")
            logger.warning("WebSocket connection rejected: another client already connected")
            return
        manager.websocket_connected.value = 1
    
    logger.info("WebSocket connection accepted")
    
    try:
        async def send_batches():
            while manager.is_running():
                if not manager.websocket_out_queue:
                    await asyncio.sleep(0.1)
                    continue
                
                try:
                    try:
                        message = manager.websocket_out_queue.get(timeout=0.1)
                    except queue.Empty:
                        await asyncio.sleep(0.1)
                        continue
                    
                    try:
                        await asyncio.wait_for(websocket.send_json(message), timeout=5.0)
                        logger.debug(f"Sent {message.get('type')} batch to WebSocket client")
                    except asyncio.TimeoutError:
                        logger.error(f"Timeout sending {message.get('type')} batch to WebSocket client")
                        raise
                except Exception as e:
                    logger.error(f"Error sending batch via WebSocket: {e}")
                    raise
        
        async def receive_acks():
            while manager.is_running():
                try:
                    data = await websocket.receive_json()
                    if data.get("type") == "ack" and manager.websocket_ack_queue:
                        data["timestamp"] = time.time()
                        manager.websocket_ack_queue.put_nowait(data)
                        logger.info(f"Received ack for batch {data.get('id')}")
                except WebSocketDisconnect:
                    raise
                except Exception as e:
                    logger.error(f"Error receiving ack via WebSocket: {e}")
                    raise
        
        send_task = asyncio.create_task(send_batches())
        receive_task = asyncio.create_task(receive_acks())
        
        done, pending = await asyncio.wait(
            [send_task, receive_task],
            return_when=asyncio.FIRST_EXCEPTION
        )
        
        for task in pending:
            task.cancel()
        
        for task in done:
            if task.exception():
                logger.error(f"WebSocket task error: {task.exception()}")
                raise task.exception()
    
    except WebSocketDisconnect as e:
        logger.info(f"WebSocket connection disconnected by client: code={getattr(e, 'code', None)}")
    except Exception as e:
        logger.error(f"WebSocket error: {e}")
    finally:
        if manager.websocket_lock and manager.websocket_connected:
            with manager.websocket_lock:
                manager.websocket_connected.value = 0
        logger.info("WebSocket connection closed")
