import time

import firebase_admin
from firebase_admin import messaging
from sqlalchemy import select

from .store import Device, Notice


class Notifications:
    def __init__(self, store, connections):
        self.store, self.connections = store, connections

    def dispatch(self):
        with self.store.transaction() as session:
            notice = session.scalar(
                select(Notice)
                .where(Notice.delivered.is_(False), Notice.next_attempt <= time.time())
                .limit(1)
            )
            if notice is None:
                return
            devices = list(session.scalars(select(Device).where(Device.owner == notice.owner)))
            # Claim this attempt durably. A crash may cause a duplicate push.
            notice.attempts += 1
            notice.next_attempt = int(time.time()) + min(3600, 15 * 2 ** min(notice.attempts, 8))
            identifier, owner, chat_id, kind = notice.id, notice.owner, notice.chat_id, notice.kind
            tokens = [(device.token, device.locale) for device in devices]
        if not tokens:
            # A future registration should not receive stale completion/questions.
            with self.store.transaction() as session:
                session.get(Notice, identifier).delivered = True
            return
        self.connections.authorize(owner)
        try:
            firebase_admin.get_app()
        except ValueError:
            firebase_admin.initialize_app()
        for token, locale in tokens:
            russian = locale.startswith("ru")
            title = "Агент Esketit" if russian else "Esketit Agent"
            body = {
                "question": "Нужен ваш ответ" if russian else "Your answer is needed",
                "complete": "Работа агента завершена" if russian else "The agent finished its work",
                "review": "Обновлён статус проверки" if russian else "Submission review updated",
            }[kind]
            try:
                messaging.send(
                    messaging.Message(
                        token=token,
                        notification=messaging.Notification(title=title, body=body),
                        data={"chatId": chat_id, "noticeId": identifier},
                        apns=messaging.APNSConfig(
                            headers={"apns-collapse-id": chat_id},
                            payload=messaging.APNSPayload(messaging.Aps(sound="default")),
                        ),
                    )
                )
            except messaging.UnregisteredError:
                with self.store.transaction() as session:
                    device = session.get(Device, token)
                    if device and device.owner == owner:
                        session.delete(device)
        with self.store.transaction() as session:
            session.get(Notice, identifier).delivered = True
