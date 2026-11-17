package session

import "github.com/GregoryGost/gost-restreamer/internal/core"

// commandFailure сохраняет доказанную типизированную причину cause либо описывает отказ известной
// стадии команды c. Идентификаторы берутся только из принятой сервером квитанции; нативная,
// аппаратная или сетевая первопричина из диагностического текста не выводится. nil означает успех.
func commandFailure(c core.CommandReceipt, cause error) error {
	if cause == nil || core.FailureReasonFrom(cause) != nil {
		return cause
	}
	// reason накапливает код известной стадии и идентификаторы принятой команды.
	reason := core.FailureReason{Context: core.FailureContext{SessionID: c.SessionID, OutputID: c.OutputID, CommandID: c.ID}}
	switch c.Kind {
	case core.CommandStartSession:
		reason.Code = core.FailureSessionStartFailed
		reason.Context.StreamID = c.TargetID
	case core.CommandAddOutput, core.CommandStartOutput:
		reason.Code = core.FailureOutputStartFailed
	case core.CommandStopSession:
		reason.Code = core.FailureSessionStopFailed
	case core.CommandStopOutput, core.CommandDetachOutput:
		reason.Code = core.FailureOutputStopFailed
	case core.CommandPauseSession:
		reason.Code = core.FailureSessionPauseFailed
	case core.CommandResumeSession:
		reason.Code = core.FailureSessionResumeFailed
	default:
		return cause
	}
	return core.WithFailureReason(cause, reason)
}

// manifestFailureContext возвращает публичные идентификаторы закреплённого manifest без пути,
// хешей и параметров профиля. Manifest поступает из самостоятельного снимка выхода.
func manifestFailureContext(manifest core.ArtifactManifest) core.FailureContext {
	return core.FailureContext{AssetID: manifest.AssetID, GenerationID: manifest.GenerationID, VariantID: manifest.VariantID}
}

// outputFailureContext дополняет идентификаторы артефакта прочитанными Session/Output и
// самостоятельными снимками группы, профиля и назначения output.
func outputFailureContext(output core.SessionOutput) core.FailureContext {
	// result содержит доказанные идентификаторы снимка, дополняемые родителем и самим выходом.
	result := manifestFailureContext(output.Snapshot.Artifact)
	result.SessionID, result.OutputID = output.SessionID, output.ID
	result.GroupID, result.ProfileID = output.Snapshot.Group.ID, output.Snapshot.Profile.ID
	result.DestinationID = output.Snapshot.Destination.ID
	return result
}

// obsSessionActive разрешает прежний bare ErrInUse и ровно собственную верхнеуровневую
// session_active с непосредственным ErrInUse. Произвольное оборачивание и составные ошибки
// не дают OBS разрешения на повторный выбор активного запуска.
func obsSessionActive(err error) bool {
	if err == core.ErrInUse {
		return true
	}
	// reason содержит только непосредственную собственную оболочку; ok исключает любые
	// обёртки и joins, в которых такая причина находится глубже.
	reason, ok := err.(*core.ReasonError)
	return ok && reason != nil && reason.Reason().Code == core.FailureSessionActive && reason.Unwrap() == core.ErrInUse
}
