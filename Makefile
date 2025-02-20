
BUILD_TOOLS = $(PWD)/../build-tools

.PHONY: build-tools
build-tools:
	# If the 'build-tools' directory does not exist:
	# - The -include directive at the bottom will not load ../build-tools/Makefile.common because the path does not exist.
	# - The first makefile target that appears in this makefile will be executed (build-tools)
	# - The 'build-tools' target will clone the 'build-tools' repository to get the common build tools
	# - make $(MAKECMDGOALS) is invoked
	#   The second time, the 'include' directive will load Makefile.common, and the default target specified in Makefile.common
	#   will be executed.
	#
	local_branch=$$(git branch | grep \* | cut -d ' ' -f2) ; \
	echo "Local branch: '$$local_branch'" ; \
	if [ "$$(id -un)" != "jenkins" ] ; then \
		if [ -d $(BUILD_TOOLS) ] ; then \
			echo "Pulling build-tools repository..."; \
			(cd $(BUILD_TOOLS) && git pull ); \
		else \
			echo "Cloning build-tools repository..."; \
			(cd .. && git clone ssh://git@bitbucket-eng-sjc1.cisco.com:7999/an/build-tools.git ); \
			if git branch -r | grep $$local_branch ; then \
				(echo "Set build-tools branch to '$$local_branch'" && cd $(BUILD_TOOLS) && git checkout $$local_branch) ; \
			fi ; \
			make $(MAKECMDGOALS) ; \
		fi \
	else \
		echo "Jenkins CI build. Assuming dependencies have already been pulled" ; \
	fi

-include $(BUILD_TOOLS)/Makefile.include

# Include Makefile.custom if it exists. This can be used to define custom variables and rules.
-include Makefile.custom

# Include Makefile.sign-image if it exists. This can be used to sign an image
-include Makefile.sign-image

# Use the "-include" directive to ignore a makefile which does not exist, with no error message
# This happens the first time when build-tools has not been checked out yet.
-include $(BUILD_TOOLS)/Makefile.common

ifeq ("$(wildcard $(BUILD_TOOLS))", "")
.DEFAULT_GOAL := build-tools
endif

