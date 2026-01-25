import time
import os
import uuid
import requests
import queue
from requests.exceptions import RequestException
from typing import List, Optional
from multiprocessing import Process, Queue, Event, Value

from pow.data import (
    ProofBatch,
    ValidatedBatch,
    InValidation,
)
from pow.compute.controller import (
    Controller,
    Phase,
)
from common.logger import create_logger

logger = create_logger(__name__)


class Sender(Process):
    def __init__(
        self,
        url: str,
        generation_queue: Queue,
        validation_queue: Queue,
        phase: Phase,
        r_target: float,
        fraud_threshold: float,
        websocket_out_queue: Optional[Queue] = None,
        websocket_ack_queue: Optional[Queue] = None,
        websocket_connected: Optional[Value] = None,
    ):
        super().__init__()
        self.url = url
        self.phase = phase
        self.generation_queue = generation_queue
        self.validation_queue = validation_queue
        self.in_validation_queue = Queue()
        self.r_target = r_target
        self.fraud_threshold = fraud_threshold

        self.websocket_out_queue = websocket_out_queue
        self.websocket_ack_queue = websocket_ack_queue
        self.websocket_connected = websocket_connected
        self.ws_ack_timeout = float(os.getenv("POW_WS_ACK_TIMEOUT", "5.0"))

        self.in_validation: List[InValidation] = []
        self.generated_not_sent: List[ProofBatch] = []
        self.validated_not_sent: List[ValidatedBatch] = []
        self.stop_event = Event()

    def _try_send_via_websocket(self, batch_type: str, batch: dict) -> bool:
        if not self.websocket_connected or self.websocket_connected.value == 0:
            return False
        
        if not self.websocket_out_queue or not self.websocket_ack_queue:
            return False
        
        try:
            batch_id = str(uuid.uuid4())
            message = {
                "type": batch_type,
                "batch": batch,
                "id": batch_id
            }
            
            try:
                self.websocket_out_queue.put_nowait(message)
            except queue.Full:
                logger.debug("WebSocket queue is full, falling back to HTTP")
                return False
            
            logger.info(f"Sent {batch_type} batch via WebSocket, waiting for ack")
            
            timeout = self.ws_ack_timeout
            start_time = time.time()
            collected_acks = []
            max_ack_age = timeout * 2
            
            while time.time() - start_time < timeout:
                try:
                    ack = self.websocket_ack_queue.get(timeout=0.1)
                    if ack.get("id") == batch_id:
                        logger.info(f"Received ack for {batch_type} batch via WebSocket")
                        for stale_ack in collected_acks:
                            try:
                                self.websocket_ack_queue.put_nowait(stale_ack)
                            except queue.Full:
                                logger.debug(f"Could not re-queue ACK {stale_ack.get('id')}: queue full")
                        return True
                    else:
                        ack_age = time.time() - ack.get("timestamp", start_time)
                        if ack_age < max_ack_age:
                            collected_acks.append(ack)
                        else:
                            logger.debug(f"Discarding stale ACK {ack.get('id')} (age: {ack_age:.1f}s)")
                except queue.Empty:
                    pass
            
            logger.warning(f"Timeout waiting for ack for {batch_type} batch via WebSocket (waited {timeout}s)")
            for stale_ack in collected_acks:
                try:
                    self.websocket_ack_queue.put_nowait(stale_ack)
                except queue.Full:
                    logger.debug(f"Could not re-queue ACK {stale_ack.get('id')}: queue full")
            return False
        except Exception as e:
            logger.error(f"Error sending {batch_type} batch via WebSocket: {e}")
            return False

    def _send_generated(self):
        if not self.generated_not_sent:
            return

        failed_batches = []

        for batch in self.generated_not_sent:
            sent = self._try_send_via_websocket("generated", batch.__dict__)
            
            if not sent:
                try:
                    logger.info("WebSocket fallback to HTTP for generated batch")
                    logger.info(f"Sending generated batch to {self.url} via HTTP")
                    response = requests.post(
                        f"{self.url}/generated",
                        json=batch.__dict__,
                    )
                    response.raise_for_status()
                    logger.info("Successfully sent generated batch via HTTP")
                except RequestException as e:
                    failed_batches.append(batch)
                    logger.error(f"Error sending generated batch to {self.url}: {e}")

        self.generated_not_sent = failed_batches

    def _send_validated(self):
        if not self.validated_not_sent:
            return

        failed_batches = []

        for batch in self.validated_not_sent:
            sent = self._try_send_via_websocket("validated", batch.__dict__)
            
            if not sent:
                try:
                    logger.info("WebSocket fallback to HTTP for validated batch")
                    logger.info(f"Sending validated batch to {self.url} via HTTP")
                    response = requests.post(
                        f"{self.url}/validated",
                        json=batch.__dict__,
                    )
                    response.raise_for_status()
                    logger.info("Successfully sent validated batch via HTTP")
                except RequestException as e:
                    failed_batches.append(batch)
                    logger.error(f"Error sending validated batch to {self.url}: {e}")

        self.validated_not_sent = failed_batches

    def _get_generated(self) -> ProofBatch:
        batches = [
            ProofBatch.merge(
                Controller.get_from_queue(self.generation_queue)
            )
        ]
        return ProofBatch.merge(batches)

    def _get_validated(self) -> List[ValidatedBatch]:
        batches = Controller.get_from_queue(self.validation_queue)
        in_validation = self._get_in_validation()
        for batch in batches:
            for in_val in in_validation:
                in_val.process(batch)

        in_validation_ready = [
            in_val.validated(self.r_target, self.fraud_threshold)
            for in_val in in_validation
            if in_val.is_ready()
        ]
        return in_validation_ready

    def _get_in_validation(self) -> List[InValidation]:
        batches = Controller.get_from_queue(self.in_validation_queue)
        batches = [
            InValidation(batch)
            for batch in batches
        ]
        self.in_validation.extend(batches)
        return self.in_validation

    def run(self):
        logger.info("Sender started")
        while not self.stop_event.is_set():
            if self.phase.value == Phase.GENERATE:
                generated = self._get_generated()
                if len(generated) > 0:
                    self.generated_not_sent.append(generated)
                self._send_generated()

            elif self.phase.value == Phase.VALIDATE:
                self.validated_not_sent.extend(self._get_validated())
                self.in_validation = [
                    b for b in self.in_validation
                    if not b.is_ready()
                ]
                self._send_validated()

            time.sleep(5)
        logger.info("Sender stopped")

    def stop(self):
        self.stop_event.set()
