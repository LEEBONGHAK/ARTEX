package db

import "database/sql"

// Settings is a tiny key-value store for global app config the UI toggles at
// runtime (e.g. traffic_capture). Missing keys fall back to caller defaults.

// GetSetting returns the stored value and ok=false when the key is unset.
func (d *DB) GetSetting(key string) (value string, ok bool, err error) {
	err = d.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts a setting value.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

// InsertSettingIfAbsent 는 key 가 아직 존재하지 않을 때만 쓰고, 이미 존재하면 그대로 두고
// inserted=false 를 반환한다. auth.password_hash 처럼 "최초 1회만 설정 허용"하는 키에 쓴다:
// 판정은 데이터베이스의 기본 키 제약에 달려 있으므로, 호출 측이 GetSetting 으로 먼저 확인한 뒤
// 쓰는 식의 검사는 그저 빠른 실패 경로일 뿐이다 — 읽기 오류나 동시성 충돌이 나더라도 기존 값이
// ON CONFLICT DO UPDATE 로 얼떨결에 덮어써지지 않는다.
func (d *DB) InsertSettingIfAbsent(key, value string) (inserted bool, err error) {
	res, err := d.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING`, key, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetBool returns the boolean setting, or def when unset/unparseable.
func (d *DB) GetBool(key string, def bool) bool {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v == "true" || v == "1"
}

// SetBool stores a boolean setting as "true"/"false".
func (d *DB) SetBool(key string, val bool) error {
	if val {
		return d.SetSetting(key, "true")
	}
	return d.SetSetting(key, "false")
}
