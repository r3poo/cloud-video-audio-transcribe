//go:build windows

package ui

import (
	"fmt"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"

	"cloudtranscribe/internal/openrouter"
)

// pickModelDialog asks the user which transcription model to send audio to.
// It is shown on first start (no remembered choice) and from the model row.
func (a *app) pickModelDialog() {
	if len(a.models) == 0 {
		return
	}

	var dlg *walk.Dialog
	var lb *walk.ListBox
	var lblInfo *walk.Label

	info := func(index int) string {
		if index < 0 || index >= len(a.models) {
			return " "
		}
		model := a.models[index]
		text := fmt.Sprintf("%s  ·  %s  ·  %s", model.ID, model.PriceLabel(), model.InputLabel())
		if !model.AcceptsAudio() {
			text += "   ⚠ this model may not accept audio input"
		}
		return text
	}

	current := a.cmbModel.CurrentIndex()
	if current < 0 || current >= len(a.models) {
		current = openrouter.DefaultModelIndex(a.models)
	}

	accepted := false
	if err := (declarative.Dialog{
		AssignTo: &dlg,
		Title:    "Choose a transcription model",
		MinSize:  declarative.Size{Width: 660, Height: 440},
		Size:     declarative.Size{Width: 720, Height: 500},
		Layout:   declarative.VBox{Spacing: 8},
		Children: []declarative.Widget{
			declarative.Label{
				Text: "These models were loaded from the OpenRouter API. Audio and video are sent\n" +
					"to the model you pick. Prices are per million input tokens as published by OpenRouter.",
			},
			declarative.ListBox{
				AssignTo:              &lb,
				Model:                 modelLabels(a.models),
				StretchFactor:         1,
				OnCurrentIndexChanged: func() { lblInfo.SetText(info(lb.CurrentIndex())) },
				OnItemActivated: func() {
					accepted = true
					dlg.Accept()
				},
			},
			declarative.Label{
				AssignTo: &lblInfo,
				Text:     info(current),
			},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 8},
				Children: []declarative.Widget{
					declarative.HSpacer{},
					declarative.PushButton{
						Text: "Use this model",
						OnClicked: func() {
							accepted = true
							dlg.Accept()
						},
					},
					declarative.PushButton{
						Text:      "Cancel",
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(a.mw); err != nil {
		a.appendLog("Could not open the model picker: " + err.Error())
		return
	}

	_ = lb.SetCurrentIndex(current)
	lb.SetFocus()
	dlg.Run()
	dlg.Dispose()

	index := lb.CurrentIndex()
	if !accepted || index < 0 || index >= len(a.models) {
		a.appendLog("Model selection unchanged.")
		return
	}

	_ = a.cmbModel.SetCurrentIndex(index)
	model := a.models[index]
	a.remember(keyModel, model.ID)
	a.appendLog("Model set to " + model.ID)
	a.updateRunButton()
}
