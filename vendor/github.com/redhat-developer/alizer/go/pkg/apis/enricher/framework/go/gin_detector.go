/*******************************************************************************
 * Copyright (c) 2022 Red Hat, Inc.
 * Distributed under license by Red Hat, Inc. All rights reserved.
 * This program is made available under the terms of the
 * Eclipse Public License v2.0 which accompanies this distribution,
 * and is available at http://www.eclipse.org/legal/epl-v20.html
 *
 * Contributors:
 * Red Hat, Inc.
 ******************************************************************************/
package recognizer

import (
	"github.com/redhat-developer/alizer/go/pkg/apis/language"
	"golang.org/x/mod/modfile"
)

type GinDetector struct{}

func (e GinDetector) DoFrameworkDetection(language *language.Language, goMod *modfile.File) {
	if hasFramework(goMod.Require, "github.com/gin-gonic/gin") {
		language.Frameworks = append(language.Frameworks, "Gin")
	}
}
