from clock import is_expired, now


class Session:
    def __init__(self, session_id, issued_at, ttl):
        self.id = session_id
        self.issued_at = issued_at
        self.ttl = ttl


class SessionStore:
    """内存会话存储。过期会话不应被读取或刷新。"""

    def __init__(self, ttl=300):
        self.ttl = ttl
        self._sessions = {}

    def issue(self, session_id):
        session = Session(session_id, now(), self.ttl)
        self._sessions[session_id] = session
        return session

    def get(self, session_id):
        return self._sessions.get(session_id)

    def refresh(self, session_id):
        session = self._sessions.get(session_id)
        if session is None:
            return None
        session.ttl = self.ttl
        return session
